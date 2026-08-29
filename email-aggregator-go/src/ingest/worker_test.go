package ingest_test

// worker_test 验证 IngestWorker 的事件驱动摄取消费管线（深化文档 §13 时序的消费端），
// 全部使用 InMemory 实现（零外部依赖），断言：落库、索引、通知、mail-index 发布、幂等。
// ADR-009 多租户：补充 TenantID 全链路断言——事件携带 + 落库 + 索引 + 通知均按租户隔离，
// 并加跨租户负向断言（其他租户看不见本租户邮件）。
//
// 运行：go test ./src/ingest/...
import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"email-aggregator-go/src/events"
	"email-aggregator-go/src/ingest"
	"email-aggregator-go/src/notify"
	"email-aggregator-go/src/search"
	"email-aggregator-go/src/store"
	"email-aggregator-go/src/tenant"
)

func TestIngestWorker_FullPipeline(t *testing.T) {
	ctx := context.Background()

	// ADR-009：使用非默认租户 tenant_test，凸显多租户隔离证据
	const tid = "tenant_test"

	// 全部 InMemory 实现（与真实 PG/Kafka/OpenSearch 接口一致，可无缝替换）
	bus := events.NewInMemoryBus()
	metadata := store.NewInMemoryMetadataStore()
	content := store.NewInMemoryContentStore()
	index := search.NewInMemorySearchIndex()
	notifier := notify.NewInMemoryNotifier()

	// 捕获 worker 发布的 mail-index 事件（订阅需早于发布才生效）
	var (
		mu        sync.Mutex
		mailIndex []events.MailIndexEvent
		auditEvts []events.AuditEvent
	)
	_ = bus.Subscribe(ctx, events.TopicMailIndex, "test-assert", func(_ context.Context, env events.EventEnvelope) error {
		var e events.MailIndexEvent
		if err := json.Unmarshal(env.Payload, &e); err != nil {
			return err
		}
		mu.Lock()
		mailIndex = append(mailIndex, e)
		mu.Unlock()
		return nil
	})
	_ = bus.Subscribe(ctx, events.TopicAudit, "test-assert", func(_ context.Context, env events.EventEnvelope) error {
		var e events.AuditEvent
		if err := json.Unmarshal(env.Payload, &e); err != nil {
			return err
		}
		mu.Lock()
		auditEvts = append(auditEvts, e)
		mu.Unlock()
		return nil
	})

	// 装配并启动消费管线
	w := ingest.NewIngestWorker(bus, metadata, content, index, notifier)
	if err := w.Start(ctx); err != nil {
		t.Fatalf("worker.Start: %v", err)
	}

	// 模拟采集侧发布一条 mail-ingested（携带索引所需字段 + ADR-009 TenantID）
	ev := events.MailIngestedEvent{
		TenantID:     tid,
		AccountID:    "acc_test",
		MailID:       "mail_1",
		Folder:       "INBOX",
		From:         "alice@example.com",
		Subject:      "Quarterly Report",
		BodyText:     "Please review the quarterly financial report before Friday.",
		InternalDate: 1700000000,
		SizeBytes:    123,
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// ADR-009：分区键/幂等键使用 tenant 前缀（与 orchestrator.buildMailIngested 一致）
	if err := bus.Publish(ctx, events.TopicMailIngested, tid+":"+ev.AccountID+":"+ev.MailID, payload); err != nil {
		t.Fatalf("publish mail-ingested: %v", err)
	}

	// ─── ADR-009 多租户断言 ────────────────────────────────────────────────

	// 断言 1：元数据已落库到 tenant_test 命名空间（Count==1）
	n, err := metadata.Count(tid, "acc_test")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("metadata count under tenant=%s = %d, want 1", tid, n)
	}

	// 断言 1.b（跨租户负向）：default 租户应看不见 tenant_test 的邮件
	nDefault, _ := metadata.Count(tenant.DefaultTenantID, "acc_test")
	if nDefault != 0 {
		t.Fatalf("isolation BROKEN: default tenant sees %d mails of tenant=%s, want 0", nDefault, tid)
	}

	// 断言 2：可被检索索引命中（按 subject/body 关键词，tenant_test 命名空间）
	hits, err := index.Search(tid, "acc_test", "report", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("search('report') under tenant=%s hits = %d, want 1", tid, len(hits))
	}
	if len(hits) > 0 && hits[0].TenantID != tid {
		t.Fatalf("hit.TenantID = %q, want %q (index leaked tenant)", hits[0].TenantID, tid)
	}

	// 断言 2.b（跨租户负向）：default 租户搜索同一账号应 0 命中
	hitsDefault, _ := index.Search(tenant.DefaultTenantID, "acc_test", "report", 10)
	if len(hitsDefault) != 0 {
		t.Fatalf("isolation BROKEN: default tenant search returns %d hits of tenant=%s, want 0", len(hitsDefault), tid)
	}

	// 断言 3：实时通知已发出（new-mail），且 payload.TenantID 透传至 tid
	rec := notifier.Received()
	if len(rec) != 1 || rec[0].Kind != notify.KindNewMail {
		t.Fatalf("notifications = %v, want exactly 1 new-mail", rec)
	}
	if rec[0].TenantID != tid {
		t.Fatalf("notification.TenantID = %q, want %q (tenant not propagated to notify)", rec[0].TenantID, tid)
	}

	// 断言 4：worker 已发布 mail-index，且事件携带 TenantID
	mu.Lock()
	gotIdx := len(mailIndex)
	var idxTenant string
	if gotIdx > 0 {
		idxTenant = mailIndex[0].TenantID
	}
	mu.Unlock()
	if gotIdx != 1 {
		t.Fatalf("mail-index events = %d, want 1", gotIdx)
	}
	if idxTenant != tid {
		t.Fatalf("mail-index event.TenantID = %q, want %q", idxTenant, tid)
	}

	// 断言 4.b：audit 事件携带 TenantID（合规：审计按租户分区）
	mu.Lock()
	gotAudit := len(auditEvts)
	var auditTenant string
	if gotAudit > 0 {
		auditTenant = auditEvts[0].TenantID
	}
	mu.Unlock()
	if gotAudit != 1 {
		t.Fatalf("audit events = %d, want 1", gotAudit)
	}
	if auditTenant != tid {
		t.Fatalf("audit event.TenantID = %q, want %q", auditTenant, tid)
	}

	// 断言 5：幂等——同邮件重复摄取，落库不新增，但消费管线仍重跑（mail-index 再发一次）
	if err := bus.Publish(ctx, events.TopicMailIngested, tid+":"+ev.AccountID+":"+ev.MailID, payload); err != nil {
		t.Fatalf("republish: %v", err)
	}
	n2, _ := metadata.Count(tid, "acc_test")
	if n2 != 1 {
		t.Fatalf("after idempotent re-ingest count under tenant=%s = %d, want 1", tid, n2)
	}
	mu.Lock()
	gotIdx2 := len(mailIndex)
	mu.Unlock()
	if gotIdx2 != 2 {
		t.Fatalf("mail-index events after re-ingest = %d, want 2", gotIdx2)
	}
}
