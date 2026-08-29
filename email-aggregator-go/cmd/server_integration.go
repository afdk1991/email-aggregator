//go:build integration

// Command server_integration 真实集成版常驻服务（构建标签 integration 下才编译）。
// 启动后：REST（健康/邮件/检索）+ WebSocket（实时推送）同端口；
// Kafka 的 notifications 主题实时转发到 WS 客户端。
//
// 运行：
//
//	go build -tags integration ./... && go run -tags integration ./cmd/server_integration.go
//
// 依赖 deploy/docker-compose.yml 提供的 PG / MinIO / OpenSearch / Redpanda。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"email-aggregator-go/src/aigateway"
	"email-aggregator-go/src/api"
	"email-aggregator-go/src/events"
	"email-aggregator-go/src/ingest"
	"email-aggregator-go/src/integration"
	"email-aggregator-go/src/model"
	"email-aggregator-go/src/notify"
	"email-aggregator-go/src/tenant"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envPort 读取端口型环境变量；为空或非法时回退默认值，避免启动因配置笔误而失败。
func envPort(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > 65535 {
		fmt.Printf("[warn] %s=%q 非法，回退默认端口 %d\n", key, v, def)
		return def
	}
	return n
}

func main() {
	ctx := context.Background()

	// ADR-004 运行期落地：用 KMS 信封加密保险库解析敏感凭据（明文兼容 / 信封拆封）。
	vault := newCredentialVault()
	cfg := integration.Config{
		PGDSN:           env("PG_DSN", "postgres://agg:agg@postgres:5432/agg"),
		ObjectEndpoint:  env("MINIO_ENDPOINT", "minio:9000"),
		ObjectBucket:    env("MINIO_BUCKET", "agg-mail"),
		ObjectAccessKey: env("MINIO_ACCESS_KEY", "agg"),
		ObjectSecretKey: env("MINIO_SECRET_KEY", "agg-secret"),
		ObjectSecure:    os.Getenv("MINIO_SECURE") == "true",
		OpenSearchAddr:  env("OPENSEARCH_ADDR", "https://opensearch:9200"),
		OpenSearchUser:  env("OPENSEARCH_USER", "admin"),
		OpenSearchPass:  env("OPENSEARCH_PASS", "Kp3mQ9@vL2*rT7xA8"),
		KafkaBrokers:    []string{env("KAFKA_BROKERS", "kafka:9092")},
	}
	// 敏感凭据经保险库解析：若 OPENSEARCH_PASS / MINIO_SECRET_KEY 为信封加密 JSON 则拆封，
	// 否则按明文使用（向后兼容 PoC）。明文仅在进程内存短暂存在、零落盘。
	var serr error
	if cfg.ObjectSecretKey, serr = resolveSecret(ctx, vault, cfg.ObjectSecretKey); serr != nil {
		panic(fmt.Errorf("MINIO_SECRET_KEY: %w", serr))
	}
	if cfg.OpenSearchPass, serr = resolveSecret(ctx, vault, cfg.OpenSearchPass); serr != nil {
		panic(fmt.Errorf("OPENSEARCH_PASS: %w", serr))
	}

	adapters, err := integration.Wire(ctx, cfg)
	if err != nil {
		panic(err)
	}
	defer adapters.Bus.Close()

	// 事件驱动摄取消费侧：订阅 mail-ingested，落库 + 内容寻址 + 索引 + 通知 + 审计。
	// 与 cmd/demo.go 共用同一 IngestWorker；Kafka 适配器在后台 goroutine 持续拉取同 group。
	worker := ingest.NewIngestWorker(adapters.Bus, adapters.Metadata, adapters.Content, adapters.Index, adapters.Notifier)
	if err := worker.Start(ctx); err != nil {
		panic(err)
	}

	// ADR-010 AI 能力路由网关：自托管（私有/企业零出境）+ 第三方（公共同意+脱敏）。
	// 审计事件发布到 Kafka audit 主题（与摄取侧 audit 同源，继承 ADR-002）。
	// 未配置真实端点（占位域名/空）时自动降级为本地 demo 回环提供方，使 /api/ai/chat 开箱可用且可验证。
	selfHosted, thirdParty := aigateway.ProvidersFromEnv(os.Getenv)
	aiRouter := aigateway.NewRouter(selfHosted, thirdParty, aigateway.NewRegexRedactor(),
		func(_ context.Context, ev aigateway.AuditEvent) {
			if payload, err := json.Marshal(ev); err == nil {
				_ = adapters.Bus.Publish(ctx, events.TopicAudit, ev.TenantID, payload)
			}
		})

	// 监听端口：默认 8080，可用环境变量 HTTP_PORT 覆盖。
	// 本机多项目并存时（项目002 的 gateway-go 固定占用 8080）需改端口以避免冲突。
	port := envPort("HTTP_PORT", 8080)

	// 连接/账户服务：把演示账户种子进注册表（幂等 upsert），保证前端 tab 与账户服务一致。
	// 演示账户不落明文凭据——credentialsRef 留空，指向后续 Token Vault/KMS 集成（ADR-004）。
	seedAccounts := []model.Account{
		{ID: "acc_demo", Provider: model.ProviderIMAP, Email: "demo@example.com", DisplayName: "Demo IMAP", Status: model.AccountActive, SyncFolder: "INBOX"},
		{ID: "acc_kafka", Provider: model.ProviderIMAP, Email: "kafka@example.com", DisplayName: "Kafka Pipeline", Status: model.AccountActive, SyncFolder: "INBOX"},
		{ID: "acc_ts", Provider: model.ProviderIMAP, Email: "ts@example.com", DisplayName: "TS Contract", Status: model.AccountActive, SyncFolder: "INBOX"},
	}
	for _, a := range seedAccounts {
		if err := adapters.Accounts.Upsert(tenant.Resolve("default"), a); err != nil {
			fmt.Printf("[warn] seed account %s: %v\n", a.ID, err)
		}
	}

	// REST + WS 同端口（含 AI 网关 /api/ai/chat、账户服务 /api/accounts）
	mux := http.NewServeMux()
	mux.Handle("/", api.NewApiServer(adapters.Metadata, adapters.Index, adapters.Notifier, port).
		WithAIGateway(aiRouter).
		WithAccounts(adapters.Accounts).
		Handler())

	// WebSocket 实时推送：通过可选接口断言解耦于具体 Notifier 实现类型。
	// 任意实现了 Upgrade(w, r, accountID) 的 Notifier 均可挂载；断言失败仅跳过 /ws 路由（不再静默 nil→500）。
	type wsUpgrader interface {
		Upgrade(w http.ResponseWriter, r *http.Request, accountID string)
	}
	if up, ok := adapters.Notifier.(wsUpgrader); ok {
		mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
			accountID := r.URL.Query().Get("accountId")
			if accountID == "" {
				http.Error(w, "accountId required", http.StatusBadRequest)
				return
			}
			up.Upgrade(w, r, accountID)
		})
	} else {
		fmt.Println("[warn] Notifier 未实现 WebSocket Upgrade，跳过 /ws 路由挂载")
	}

	// Kafka notifications → 实时推送给该账户 WS 订阅者
	// ADR-009：按事件载荷的 TenantID 路由到正确租户的 WS 连接；
	// 旧事件缺 TenantID 时回退 DefaultTenantID，保持单租户场景兼容。
	_ = adapters.Bus.Subscribe(ctx, events.TopicNotifications, "api-push", func(_ context.Context, env events.EventEnvelope) error {
		var ne events.NotificationEvent
		if err := json.Unmarshal(env.Payload, &ne); err != nil {
			return err
		}
		tid := tenant.Resolve(ne.TenantID)
		adapters.Notifier.Publish(tid, ne.AccountID, notify.NotificationPayload{
			Kind:      notify.NotifyKind(ne.Type),
			AccountID: ne.AccountID,
			Preview:   ne.MailID,
			TS:        time.Now().UnixMilli(),
		})
		return nil
	})

	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("[server] listening on %s (integration build)\n", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		panic(err)
	}
}
