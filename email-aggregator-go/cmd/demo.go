//go:build !integration

// Command demo 端到端演示（零外部依赖，本地即可跑通核心契约）。
// 运行：go run ./cmd/demo.go
// 注：与 server_integration.go（//go:build integration）互斥，避免同一 package main 重复声明。
//
// 全链路：连接器注册表 → IMAP 适配器 → 7 态同步状态机 → KMS 信封加密
//
//	→ 编排器驱动 → 全量采集(sink) 发布 mail-ingested
//	→ IngestWorker 消费（落库+内容寻址+索引+发布 mail-index+通知 WS+审计）
//	→ mail-index / audit 订阅者打印证据 → 检索/统计验证。
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"email-aggregator-go/src/connector"
	"email-aggregator-go/src/events"
	"email-aggregator-go/src/ingest"
	"email-aggregator-go/src/model"
	"email-aggregator-go/src/notify"
	"email-aggregator-go/src/search"
	"email-aggregator-go/src/security"
	"email-aggregator-go/src/store"
	"email-aggregator-go/src/syncsvc"
	"email-aggregator-go/src/tenant"
)

func main() {
	ctx := context.Background()

	// ADR-009 演示租户：使用非默认 tenantA，凸显多租户透传证据
	const demoTenant = "tenantA"

	// 1) 内存邮件源（模拟 IMAP 服务端），含 From/Subject/BodyText 供索引
	//    种子邮件携带 TenantID（仅为表明意图；真实采集时 busSink 会强制盖戳覆盖）
	server := connector.NewInMemoryMailServer()
	server.Add(model.CanonicalMail{TenantID: demoTenant, ID: "m1", AccountID: "acc_demo", Folder: "INBOX",
		From: model.Address{Email: "alice@example.com"}, Subject: "Welcome onboard",
		BodyText: "Welcome to the platform, your account is ready.", InternalDate: 1700000000, Cursor: model.SyncCursor{LastUID: 1}})
	server.Add(model.CanonicalMail{TenantID: demoTenant, ID: "m2", AccountID: "acc_demo", Folder: "INBOX",
		From: model.Address{Email: "bob@vendor.com"}, Subject: "Invoice #2024-03",
		BodyText: "Please find attached the invoice for March services.", InternalDate: 1700000100, Cursor: model.SyncCursor{LastUID: 2}})
	server.Add(model.CanonicalMail{TenantID: demoTenant, ID: "m3", AccountID: "acc_demo", Folder: "INBOX",
		From: model.Address{Email: "carol@meet.com"}, Subject: "Team sync meeting",
		BodyText: "Reminder: weekly team sync at 10am tomorrow.", InternalDate: 1700000200, Cursor: model.SyncCursor{LastUID: 3}})

	// 2) 事件总线 + 下游订阅者（打印 tenant 证据）
	bus := events.NewInMemoryBus()
	_ = bus.Subscribe(ctx, events.TopicMailIngested, "trace", func(_ context.Context, env events.EventEnvelope) error {
		var e events.MailIngestedEvent
		_ = json.Unmarshal(env.Payload, &e)
		fmt.Printf("  [sub:mail-ingested] tenant=%s key=%s\n", e.TenantID, env.Key)
		return nil
	})
	_ = bus.Subscribe(ctx, events.TopicMailIndex, "audit-log", func(_ context.Context, env events.EventEnvelope) error {
		var e events.MailIndexEvent
		_ = json.Unmarshal(env.Payload, &e)
		fmt.Printf("  [sub:mail-index] tenant=%s account=%s mail=%s\n", e.TenantID, e.AccountID, e.MailID)
		return nil
	})
	_ = bus.Subscribe(ctx, events.TopicAudit, "audit-log", func(_ context.Context, env events.EventEnvelope) error {
		var e events.AuditEvent
		_ = json.Unmarshal(env.Payload, &e)
		fmt.Printf("  [sub:audit] tenant=%s %s %s target=%s\n", e.TenantID, e.Actor, e.Action, e.Target)
		return nil
	})

	// 3) 连接器注册表（ADR-005）
	reg := connector.NewConnectorRegistry()
	reg.Register(model.ProviderIMAP, func(_ model.Provider, _ map[string]string) (model.Connector, error) {
		return connector.NewIMAPConnector(server), nil
	})
	conn, err := reg.Create(model.ProviderIMAP, nil)
	if err != nil {
		panic(err)
	}

	// 4) 7 态同步状态机演示
	st, _ := syncsvc.Advance(syncsvc.StateUnconnected, syncsvc.EvAuthorize)
	st, _ = syncsvc.Advance(st, syncsvc.EvFullDone)
	st, _ = syncsvc.Advance(st, syncsvc.EvFullDone)
	fmt.Printf("[statemachine] UNCONNECTED --AUTHORIZE,FULL_DONE,FULL_DONE--> %s\n", st)
	if _, e := syncsvc.Advance(syncsvc.StateUnconnected, syncsvc.EvFullDone); e != nil {
		fmt.Printf("[statemachine] expected illegal transition: %v\n", e)
	}

	// 5) KMS 信封加密演示（ADR-004）
	kms := security.NewInMemoryKms()
	vault := security.NewCredentialVault(kms)
	env, err := vault.Seal(ctx, []byte(`{"username":"alice","password":"s3cret"}`))
	if err != nil {
		panic(err)
	}
	pt, err := vault.Unseal(ctx, env)
	if err != nil {
		panic(err)
	}
	fmt.Printf("[vault] seal->unseal ok: %s (kekId=%s)\n", pt, env.KEKID)

	// 6) IngestWorker 消费管线（落库 + 内容寻址 + 索引 + mail-index + 通知 WS + 审计）
	//    必须在编排器触发采集之前启动，否则 mail-ingested 事件无消费者。
	metadata := store.NewInMemoryMetadataStore()
	content := store.NewInMemoryContentStore()
	index := search.NewInMemorySearchIndex()
	notifier := notify.NewInMemoryNotifier()
	notifier.AddSink("acc_demo", notify.NewFuncSink(func(p notify.NotificationPayload) {
		fmt.Printf("  [ws-push] tenant=%s new-mail account=%s preview=%q\n", p.TenantID, p.AccountID, p.Preview)
	}))
	worker := ingest.NewIngestWorker(bus, metadata, content, index, notifier)
	if err := worker.Start(ctx); err != nil {
		panic(err)
	}

	// 7) 编排器接收 sync-tasks（驱动状态机 + 连接 + 全量采集 → 发布 mail-ingested）
	//    编排器内置 busSink，采集到的邮件经总线发布，IngestWorker 自动消费。
	orch := syncsvc.NewOrchestrator(syncsvc.OrchestratorDeps{
		Bus:        bus,
		Vault:      vault,
		Connector:  conn,
		Credential: model.Credential{Type: "password", Username: "alice"},
	})
	if err := orch.HandleSyncTask(ctx, events.SyncTaskEvent{
		TenantID:  demoTenant, // ADR-009：sync-tasks 携带租户，编排器据 busSink 透传至 mail-ingested
		AccountID: "acc_demo",
		Mode:      "initial",
	}); err != nil {
		panic(err)
	}

	// 8) 检索验证（按 ADR-009 隔离的 tenantA 命名空间）
	tid := tenant.Resolve(demoTenant)
	hits, _ := index.Search(tid, "acc_demo", "invoice", 10)
	fmt.Printf("[search] tenant=%s query=invoice -> %d hit(s)\n", tid, len(hits))
	for _, h := range hits {
		fmt.Printf("         - %s | %s | %s\n", h.ID, h.From, h.Subject)
	}
	all, _ := index.Search(tid, "acc_demo", "sync", 10)
	fmt.Printf("[search] tenant=%s query=sync -> %d hit(s)\n", tid, len(all))

	// 9) 落库 / 内容寻址 / 通知统计（按 tenantA 隔离）
	n, _ := metadata.Count(tid, "acc_demo")
	fmt.Printf("[store] tenant=%s total mails in metadata = %d\n", tid, n)
	rec := notifier.Received()
	fmt.Printf("[notify] total new-mail pushes = %d (all under tenant=%s)\n", len(rec), tid)
	for i, p := range rec {
		if p.TenantID != tid {
			fmt.Printf("  [notify][%d] BROKEN isolation: payload.TenantID=%q expected=%q\n", i, p.TenantID, tid)
		}
	}

	fmt.Println("demo DONE")
}
