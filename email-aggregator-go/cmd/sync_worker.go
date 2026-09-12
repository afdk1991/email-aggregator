//go:build integration && sync_worker

// Command sync_worker Phase 2 / 蓝图 §11 服务拆分：同步编排器独立进程。
//
// 职责（与 cmd/server_integration.go 解耦）：
//   - 订阅 Kafka sync-tasks 主题（消费组 sync-worker）
//   - 对每条 SyncTaskEvent：按 provider + server_host 构造 Connector →
//     Vault 拆封凭据（KMS 信封，ADR-004）→ Orchestrator.HandleSyncTask
//     （全量/增量采集，按游标决定）→ mail-ingested → 摄取管线（独立进程处理）
//   - 不暴露 HTTP，只跑消费循环；可水平扩容 N 副本（Kafka 分区并发）
//
// 运行：
//   go build -tags integration,sync_worker -o sync_worker.exe ./cmd/
//   ./sync_worker.exe
//
// 依赖：deploy/docker-compose.yml 提供的 PG/MinIO/OpenSearch/Kafka。
// 注意：sync_worker 不直接写 PG/OpenSearch/MinIO（仅用 PG 读账户注册表 + 游标，
// 用 Kafka 发布 mail-ingested）。这是 Phase 2 服务拆分的核心边界——sync 与
// ingest 不共享存储写路径，避免双写竞争。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"email-aggregator-go/src/connector"
	"email-aggregator-go/src/events"
	"email-aggregator-go/src/integration"
	"email-aggregator-go/src/model"
	"email-aggregator-go/src/observ"
	"email-aggregator-go/src/security"
	"email-aggregator-go/src/store"
	"email-aggregator-go/src/syncsvc"
	"email-aggregator-go/src/tenant"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── 配置（与 server_integration.go 同源 env 变量）──
	vault := newCredentialVault()
	cfg := integration.Config{
		PGDSN:           env("PG_DSN", "postgres://agg:agg@postgres:5432/agg"),
		KafkaBrokers:    []string{env("KAFKA_BROKERS", "kafka:9092")},
		KafkaDialAddr:   env("KAFKA_DIAL_ADDR", ""),
		OpenSearchAddr:  env("OPENSEARCH_ADDR", "https://opensearch:9200"),
		OpenSearchUser:  env("OPENSEARCH_USER", "admin"),
		OpenSearchPass:  env("OPENSEARCH_PASS", "Kp3mQ9@vL2*rT7xA8"),
	}
	// OpenSearch/Kafka 凭据可能为信封加密 → 拆封
	var serr error
	if cfg.OpenSearchPass, serr = resolveSecret(ctx, vault, cfg.OpenSearchPass); serr != nil {
		fmt.Fprintf(os.Stderr, "[sync_worker] OPENSEARCH_PASS: %v\n", serr)
		os.Exit(1)
	}

	// ── 装配（仅 PG accounts + cursor + Kafka bus，不接 metadata/content/index/notifier）──
	// 注：sync_worker 不直接写存储/索引；它只读账户注册表（判断 paused/error）与游标（增量）。
	adapters, err := integration.WireSync(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[sync_worker] wire failed: %v\n", err)
		os.Exit(1)
	}
	defer adapters.Bus.Close()

	// ── 账户同步运行时状态（独立进程的本地视图，不与 server_integration 共享内存）──
	// sync_worker 的"运行状态"仅供自身进程内观察；运维入口的 SyncStatus 仍由 webui 进程维护。
	reg := connector.NewDefaultRegistry()

	// ── 订阅 sync-tasks 主题 ──
	// 消费组 sync-worker：多副本共享同一组，Kafka 自动分区负载均衡。
	// ADR-009：每条 SyncTaskEvent 携带 TenantID；编排器据此给 mail-ingested 盖租户戳。
	groupID := env("SYNC_WORKER_GROUP", "sync-worker")
	if err := adapters.Bus.Subscribe(ctx, events.TopicSyncTasks, groupID, func(ctx context.Context, env events.EventEnvelope) error {
		return handleSyncTask(ctx, env, adapters, vault, reg)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "[sync_worker] subscribe sync-tasks failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("[sync_worker] consuming sync-tasks (group=%s)\n", groupID)
	fmt.Printf("[sync_worker] brokers=%v\n", cfg.KafkaBrokers)

	// ── 暴露 /metrics 端点供 Prometheus 抓取 ──
	// Prometheus 抓取本进程的 :8081/metrics（与 ingest_worker :8082 / search_service :8083 区分）。
	go metricsServer(envPort("SYNC_METRICS_PORT", 8081), "sync_worker")

	// ── 优雅关闭：SIGINT/SIGTERM ──
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	fmt.Println("[sync_worker] shutting down…")
	cancel()
	// 给 Kafka 消费者一点时间完成 in-flight 消息
	time.Sleep(2 * time.Second)
}

// handleSyncTask 处理一条 sync-tasks 事件：
// 1. 从账户注册表取账户（含 CredentialsRef 信封 + ServerHost）
// 2. Vault 拆封信封 → 明文凭据
// 3. 按 provider + server_host 构造 Connector
// 4. Orchestrator.HandleSyncTask（自动增量/全量）
// 5. mail-ingested 由 orchestrator.publish 发布（独立 ingest_worker 消费）
//
// 失败时把账户标为 error 态，事件返回 error 让 Kafka 重投（或转 DLQ）。
func handleSyncTask(ctx context.Context, env events.EventEnvelope, adapters *integration.SyncAdapters, vault *security.CredentialVault, reg *connector.ConnectorRegistry) error {
	var task events.SyncTaskEvent
	if err := json.Unmarshal(env.Payload, &task); err != nil {
		return fmt.Errorf("decode SyncTaskEvent: %w", err)
	}
	tid := tenant.Resolve(task.TenantID)

	acct, err := adapters.Accounts.GetAccount(tid, task.AccountID)
	if err != nil {
		observ.SyncTotal.Inc(map[string]string{"tenant": tid, "account": task.AccountID, "provider": "", "status": "error"})
		return fmt.Errorf("account lookup: %w", err)
	}
	if acct == nil {
		return fmt.Errorf("account not found: %s", task.AccountID)
	}
	if acct.CredentialsRef == "" {
		return fmt.Errorf("account %s 未配置凭据 — 跳过 sync-tasks", task.AccountID)
	}
	switch acct.Status {
	case model.AccountPaused:
		fmt.Printf("[sync_worker] account=%s status=paused -> skip\n", task.AccountID)
		return nil
	case model.AccountError:
		return fmt.Errorf("account %s status=error -> needs manual recovery", task.AccountID)
	}

	// 拆封凭据信封（ADR-004）
	var envSec security.Envelope
	if err := json.Unmarshal([]byte(acct.CredentialsRef), &envSec); err != nil {
		return fmt.Errorf("credentials envelope decode: %w", err)
	}
	pt, err := vault.Unseal(ctx, envSec)
	if err != nil {
		return fmt.Errorf("credentials unseal: %w", err)
	}
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(pt, &creds); err != nil {
		return fmt.Errorf("credentials payload decode: %w", err)
	}
	cred := model.Credential{Type: "password", Username: creds.Username, Password: creds.Password}

	// 构造真实 Connector（按 provider + server_host）
	conn, err := reg.Create(acct.Provider, map[string]string{
		"address":  acct.ServerHost,
		"useTLS":   "true",
		"endpoint": acct.ServerHost,
	})
	if err != nil {
		markAccountError(tid, acct, adapters.Accounts, err)
		return fmt.Errorf("connector create: %w", err)
	}

	// 编排器：状态机驱动连接 → 全量/增量采集 → mail-ingested
	orch := syncsvc.NewOrchestrator(syncsvc.OrchestratorDeps{
		Bus:       adapters.Bus,
		Vault:     vault,
		Connector: conn,
		Credential: cred,
		Accounts:  adapters.Accounts,
		Cursor:    adapters.Cursor,
	})
	if err := orch.HandleSyncTask(ctx, task); err != nil {
		markAccountError(tid, acct, adapters.Accounts, err)
		return fmt.Errorf("orchestrator: %w", err)
	}
	return nil
}

// markAccountError 把账户标为 error 态，避免后续 sync-tasks 反复重试同一失败账户。
func markAccountError(tid string, acct *model.Account, accounts store.AccountStore, cause error) {
	if acct.Status != model.AccountError {
		acct.Status = model.AccountError
		_ = accounts.UpdateAccount(tid, *acct)
	}
	fmt.Printf("[sync_worker] account=%s sync failed: %v\n", acct.ID, cause)
}
