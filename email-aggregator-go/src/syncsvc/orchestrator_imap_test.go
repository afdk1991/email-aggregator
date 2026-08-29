package syncsvc

import (
	"context"
	"encoding/json"
	"testing"

	"email-aggregator-go/src/connector"
	"email-aggregator-go/src/events"
	"email-aggregator-go/src/model"
)

// TestOrchestrator_IMAP_InitialFullSync_E2E 端到端验证全链路闭环：
//
//	sync-tasks 事件 → Orchestrator 状态机（UNCONNECTED→AUTHORIZING→INITIAL_FULL→INCREMENTAL）
//	  → RealIMAPConnector 连接 mock IMAP server 并拉取两封邮件
//	  → busSink 把每封邮件盖租户戳并发布为 mail-ingested 事件
//
// 同时校验 ADR-009 多租户 TenantID 透传：事件载荷携带正确的 tenantID / accountID。
func TestOrchestrator_IMAP_InitialFullSync_E2E(t *testing.T) {
	srv, stop, err := connector.StartMockIMAP()
	if err != nil {
		t.Fatalf("start mock imap: %v", err)
	}
	defer stop()

	c := connector.NewRealIMAPConnector(srv.Addr(), false, nil)
	cred := model.Credential{Type: "password", Username: "alice@example.com", Password: "pw"}

	bus := events.NewInMemoryBus()

	// 订阅 mail-ingested，收集事件以校验租户透传与内容
	var ingested []events.MailIngestedEvent
	_ = bus.Subscribe(context.Background(), events.TopicMailIngested, "test", func(_ context.Context, env events.EventEnvelope) error {
		var ev events.MailIngestedEvent
		if err := json.Unmarshal(env.Payload, &ev); err != nil {
			return err
		}
		ingested = append(ingested, ev)
		return nil
	})

	orch := NewOrchestrator(OrchestratorDeps{
		Bus:        bus,
		Connector:  c,
		Credential: cred,
	})

	task := events.SyncTaskEvent{
		TenantID:  "tenant-a",
		AccountID: "alice@example.com",
		Mode:      "initial",
	}
	if err := orch.HandleSyncTask(context.Background(), task); err != nil {
		t.Fatalf("HandleSyncTask: %v", err)
	}

	// 初始全量完成后状态机应推进到 INCREMENTAL
	if orch.State() != StateIncremental {
		t.Fatalf("expected state %s, got %s", StateIncremental, orch.State())
	}

	// 两封邮件应被发布为 mail-ingested 事件
	if len(ingested) != 2 {
		t.Fatalf("expected 2 mail-ingested events, got %d", len(ingested))
	}

	// ADR-009：TenantID / AccountID 透传
	for _, ev := range ingested {
		if ev.TenantID != "tenant-a" {
			t.Errorf("ingested TenantID = %q, want tenant-a", ev.TenantID)
		}
		if ev.AccountID != "alice@example.com" {
			t.Errorf("ingested AccountID = %q, want alice@example.com", ev.AccountID)
		}
	}

	// 内容校验：首封应解析出主题、附件标记与字节数
	if ingested[0].Subject != "测试邮件主题" {
		t.Errorf("mail1 Subject = %q, want 测试邮件主题", ingested[0].Subject)
	}
	if !ingested[0].HasAttachment {
		t.Errorf("mail1 HasAttachment should be true (doc.pdf present)")
	}
	if ingested[0].SizeBytes != 1024 {
		t.Errorf("mail1 SizeBytes = %d, want 1024", ingested[0].SizeBytes)
	}
	if ingested[1].Subject != "第二封无地址" {
		t.Errorf("mail2 Subject = %q, want 第二封无地址", ingested[1].Subject)
	}
}
