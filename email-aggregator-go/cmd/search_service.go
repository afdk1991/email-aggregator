//go:build integration && search_service

// Command search_service Phase 2 / 蓝图 §11 服务拆分：检索 HTTP 服务独立进程。
//
// 职责（与 sync_worker / ingest_worker 解耦）：
//   - 暴露 REST 检索/邮件读写端点（/api/search, /api/mails, /api/count, ...）
//   - ADR-009：按 X-Tenant-Id 头路由到正确租户的 PG shard + OpenSearch mail-<tid> 索引
//   - 只读 PG metadata + OpenSearch index；不订阅 Kafka（拉模型，无需事件循环）
//   - 不建立 MinIO/notifier（不写存储、不推 WS；通知由 ingest_worker 的 notifier 负责）
//
// 运行：
//
//	go build -tags integration,search_service -o search_service.exe ./cmd/
//	./search_service.exe
//
// 边界设计（Phase 2 核心约束）：
//   - 不与 sync_worker/ingest_worker 共享写路径（只读 metadata + index）
//   - 跨进程通信仅经 HTTP 请求/响应（无共享内存、无事件总线依赖）
//   - 可水平扩容 N 副本（无状态，前端 LB 轮询）
//
// 依赖：deploy/docker-compose.yml 提供的 PG/OpenSearch（不依赖 MinIO/Kafka）。
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"email-aggregator-go/src/api"
	"email-aggregator-go/src/integration"
	"email-aggregator-go/src/notify"
	"email-aggregator-go/src/observ"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── 配置（仅需 PG + OpenSearch，不接 MinIO/Kafka）──
	vault := newCredentialVault()
	cfg := integration.Config{
		PGDSN:          env("PG_DSN", "postgres://agg:agg@postgres:5432/agg"),
		OpenSearchAddr: env("OPENSEARCH_ADDR", "https://opensearch:9200"),
		OpenSearchUser: env("OPENSEARCH_USER", "admin"),
		OpenSearchPass: env("OPENSEARCH_PASS", "Kp3mQ9@vL2*rT7xA8"),
	}
	// OpenSearch 凭据可能为信封加密 → 拆封
	var serr error
	if cfg.OpenSearchPass, serr = resolveSecret(ctx, vault, cfg.OpenSearchPass); serr != nil {
		fmt.Fprintf(os.Stderr, "[search_service] OPENSEARCH_PASS: %v\n", serr)
		os.Exit(1)
	}

	// ── 装配（仅 PG metadata + OpenSearch index，不接 bus/content/notifier）──
	adapters, err := integration.WireSearch(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[search_service] wire failed: %v\n", err)
		os.Exit(1)
	}

	// ── HTTP 路由 ──
	// 复用 api.NewApiServer 的全部检索/邮件端点（/api/search, /api/mails, /api/count, ...）。
	// notifier 用 InMemoryNotifier 占位（search_service 不推 WS，但 ApiServer 构造需非 nil）。
	notifier := notify.NewInMemoryNotifier()
	port := envPort("SEARCH_PORT", 8083)

	mux := http.NewServeMux()
	mux.Handle("/", api.NewApiServer(adapters.Metadata, adapters.Index, notifier, port).
		WithObserv(observ.Default()).
		Handler())

	// Prometheus 抓取端点
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(observ.Default().PrometheusExposition()))
	})

	// 健康检查
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"service":"search_service"}`))
	})

	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("[search_service] listening on %s (integration build)\n", addr)
	fmt.Printf("[search_service] pg=%s os=%s\n", maskDSN(cfg.PGDSN), cfg.OpenSearchAddr)

	// ── HTTP 服务器（带优雅关闭）──
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[search_service] http server error: %v\n", err)
			os.Exit(1)
		}
	}()

	// ── 优雅关闭：SIGINT/SIGTERM ──
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	fmt.Println("[search_service] shutting down…")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
	cancel()
}
