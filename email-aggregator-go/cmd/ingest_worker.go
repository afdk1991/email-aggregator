//go:build integration && ingest_worker

// Command ingest_worker Phase 2 / 蓝图 §11 服务拆分：摄取消费侧独立进程。
//
// 职责（与 sync_worker 解耦）：
//   - 订阅 Kafka mail-ingested 主题（消费组 ingest-worker）
//   - 对每封邮件：落 PG 元数据（幂等去重）→ 落 MinIO 内容寻址 → 写 OpenSearch
//     mail-<tid> 索引 → 发布 mail-index + notifications + audit
//   - 不暴露 HTTP，只跑消费循环；可水平扩容 N 副本（Kafka 分区并发）
//
// 运行：
//
//	go build -tags integration,ingest_worker -o ingest_worker.exe ./cmd/
//	./ingest_worker.exe
//
// 边界设计（Phase 2 核心约束）：
//   - 不接 accounts/cursor（账户状态与游标由 sync_worker 维护）
//   - 不与 sync_worker 共享写路径（PG metadata 由本进程独占写）
//   - 跨进程通信仅经 Kafka 事件总线（mail-ingested → mail-index/notifications/audit）
//
// 依赖：deploy/docker-compose.yml 提供的 PG/MinIO/OpenSearch/Kafka。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"email-aggregator-go/src/ingest"
	"email-aggregator-go/src/integration"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── 配置（与 server_integration.go 同源 env 变量）──
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
		KafkaDialAddr:   env("KAFKA_DIAL_ADDR", ""),
	}
	// 敏感凭据经保险库拆封（明文兼容）
	var serr error
	if cfg.ObjectSecretKey, serr = resolveSecret(ctx, vault, cfg.ObjectSecretKey); serr != nil {
		fmt.Fprintf(os.Stderr, "[ingest_worker] MINIO_SECRET_KEY: %v\n", serr)
		os.Exit(1)
	}
	if cfg.OpenSearchPass, serr = resolveSecret(ctx, vault, cfg.OpenSearchPass); serr != nil {
		fmt.Fprintf(os.Stderr, "[ingest_worker] OPENSEARCH_PASS: %v\n", serr)
		os.Exit(1)
	}

	// ── 装配（仅 PG metadata + MinIO content + OpenSearch index + WS notifier + Kafka）──
	adapters, err := integration.WireIngest(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ingest_worker] wire failed: %v\n", err)
		os.Exit(1)
	}
	defer adapters.Bus.Close()

	// ── 摄取 worker：订阅 mail-ingested，每封邮件走完 落库→索引→通知 全链路 ──
	worker := ingest.NewIngestWorker(adapters.Bus, adapters.Metadata, adapters.Content, adapters.Index, adapters.Notifier)
	if err := worker.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "[ingest_worker] start ingest worker failed: %v\n", err)
		os.Exit(1)
	}

	groupID := env("INGEST_WORKER_GROUP", "ingest-worker")
	fmt.Printf("[ingest_worker] consuming mail-ingested (group=%s)\n", groupID)
	fmt.Printf("[ingest_worker] brokers=%v pg=%s os=%s minio=%s\n",
		cfg.KafkaBrokers, maskDSN(cfg.PGDSN), cfg.OpenSearchAddr, cfg.ObjectEndpoint)

	// ── 暴露 /metrics 端点供 Prometheus 抓取 ──
	// Prometheus 抓取本进程的 :8082/metrics（与 sync_worker :8081 / search_service :8083 区分）。
	go metricsServer(envPort("INGEST_METRICS_PORT", 8082), "ingest_worker")

	// ── 优雅关闭：SIGINT/SIGTERM ──
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	fmt.Println("[ingest_worker] shutting down…")
	cancel()
	time.Sleep(2 * time.Second)
}
