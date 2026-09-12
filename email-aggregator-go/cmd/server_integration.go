//go:build integration && !sync_worker && !ingest_worker && !search_service

// Command server_integration 真实集成版常驻服务（构建标签 integration 下才编译）。
// 启动后：REST（健康/邮件/检索）+ WebSocket（实时推送）同端口；
// Kafka 的 notifications 主题实时转发到 WS 客户端。
//
// Phase 2 / 蓝图 §11 服务拆分后，本入口为「单进程一体化模式」——当未启用
// sync_worker / ingest_worker / search_service 任一额外标签时编译。
// 拆分模式分别见 cmd/sync_worker.go / cmd/ingest_worker.go / cmd/search_service.go。
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
	"time"

	"email-aggregator-go/src/aigateway"
	"email-aggregator-go/src/api"
	"email-aggregator-go/src/connector"
	"email-aggregator-go/src/events"
	"email-aggregator-go/src/ingest"
	"email-aggregator-go/src/integration"
	"email-aggregator-go/src/model"
	"email-aggregator-go/src/notify"
	"email-aggregator-go/src/observ"
	"email-aggregator-go/src/tenant"
)

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
		// 可选拨号重写：broker advertised 通告容器名而本进程在宿主机无法解析时，指向宿主可达地址。
		KafkaDialAddr:    env("KAFKA_DIAL_ADDR", ""),
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
	// acc_139 为真实邮箱接入点：server_host=imap.139.com:993，凭据经「设凭据」接口信封加密录入。
	seedAccounts := []model.Account{
		{ID: "acc_demo", Provider: model.ProviderIMAP, Email: "demo@example.com", DisplayName: "Demo IMAP", Status: model.AccountActive, SyncFolder: "INBOX"},
		{ID: "acc_kafka", Provider: model.ProviderIMAP, Email: "kafka@example.com", DisplayName: "Kafka Pipeline", Status: model.AccountActive, SyncFolder: "INBOX"},
		{ID: "acc_ts", Provider: model.ProviderIMAP, Email: "ts@example.com", DisplayName: "TS Contract", Status: model.AccountActive, SyncFolder: "INBOX"},
		{ID: "acc_graph", Provider: model.ProviderGraph, Email: "graph@m365.example.com", DisplayName: "M365 Graph", Status: model.AccountActive, SyncFolder: "INBOX"},
		{ID: "acc_139", Provider: model.ProviderIMAP, Email: "15726833367@139.com", DisplayName: "139 邮箱", Status: model.AccountActive, SyncFolder: "INBOX", ServerHost: "imap.139.com:993"},
	}
	for _, a := range seedAccounts {
		// 幂等种子但「已存在即跳过」：不覆盖用户录入的凭据/状态/连接端点，
		// 避免每次重启把真实邮箱的 credentialsRef（KMS 信封）冲掉。
		if existing, err := adapters.Accounts.GetAccount(tenant.Resolve("default"), a.ID); err == nil && existing != nil {
			continue
		}
		if err := adapters.Accounts.Upsert(tenant.Resolve("default"), a); err != nil {
			fmt.Printf("[warn] seed account %s: %v\n", a.ID, err)
		}
	}

	// 账户真实同步执行器：手动「立即同步」走 连接器注册表 + Vault 拆封凭据 + 编排器。
	syncer := newAccountSyncer(adapters.Bus, vault, adapters.Accounts, adapters.Metadata, connector.NewDefaultRegistry())

	// REST + WS 同端口（含 AI 网关 /api/ai/chat、账户服务 /api/accounts、凭据/同步入口）
	// Phase 3 / ADR-009：可选挂载 OIDC SSO + RBAC 鉴权中间件。
	// assembleAuthMiddleware 在 sso 构建标签下读取 OIDC_* 环境变量构造验签器；
	// 未配置或非 sso 构建时返回 nil，服务器回退至 PoC 无鉴权模式（向后兼容）。
	authMiddleware := assembleAuthMiddleware(ctx, adapters.Bus)
	apiSrv := api.NewApiServer(adapters.Metadata, adapters.Index, adapters.Notifier, port).
		WithAIGateway(aiRouter).
		WithAccounts(adapters.Accounts).
		WithCredentialVault(vault).
		WithAccountSyncer(syncer).
		WithObserv(observ.Default())
	if authMiddleware != nil {
		apiSrv = apiSrv.WithAuth(authMiddleware)
	}
	mux := http.NewServeMux()
	mux.Handle("/", apiSrv.Handler())

	// 可观测性：Prometheus 抓取端点（对齐蓝图 §10 Prometheus + Grafana）。
	// 与 /api/metrics（JSON 快照，供调试）并存；此处为 Prometheus 文本 exposition 格式。
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(observ.Default().PrometheusExposition()))
	})

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
