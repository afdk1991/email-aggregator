//go:build integration

// Package main 共享辅助（仅 integration 构建可见）。
//
// Phase 2 / 蓝图 §11 服务拆分后，sync_worker / ingest_worker / search_service /
// server_integration 四个独立入口共享同一组配置解析与指标端点工具，集中放此文件，
// 避免在每个 cmd 文件里重复定义导致「同包多 main」编译期冲突。
//
// env / envPort / maskDSN 无外部依赖；metricsServer 依赖 observ.PrometheusExposition
// （integration 构建标签下可见），故本文件整体带 //go:build integration。
package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"

	"email-aggregator-go/src/observ"
)

// env 读取字符串型环境变量；为空时回退默认值。
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

// metricsServer 启动轻量 HTTP 端点暴露 observ 指标 + 健康检查。
// serviceName 用于 /health 返回体标识当前进程（sync_worker / ingest_worker / search_service）。
// 各 worker 进程独立端口（sync=8081 / ingest=8082 / search=8083），Prometheus 单独抓取。
func metricsServer(port int, serviceName string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(observ.Default().PrometheusExposition()))
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"service":"` + serviceName + `"}`))
	})
	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("[%s] metrics on %s\n", serviceName, addr)
	_ = http.ListenAndServe(addr, mux)
}

// maskDSN 隐藏密码避免日志泄露（postgres://user:pass@host:5432/db → postgres://user:***@host:5432/db）。
func maskDSN(dsn string) string {
	for i := 0; i < len(dsn); i++ {
		if dsn[i] == '@' {
			for j := 0; j < i; j++ {
				if dsn[j] == ':' && j > 0 && dsn[j-1] != '/' {
					return dsn[:j+1] + "***" + dsn[i:]
				}
			}
			return dsn[:i] + "***" + dsn[i:]
		}
	}
	return dsn
}
