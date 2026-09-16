//go:build integration && !sso && !sync_worker && !ingest_worker && !search_service

// Phase 3 / ADR-009：OIDC 装配占位（未启用 sso 标签）。
//
// 不引入 coreos/go-oidc / oauth2 依赖，保持默认 `go build -tags integration ./...` 的
// 零外部依赖能力。返回 nil 让 server_integration.go 走 PoC 无鉴权模式。
// 启用 SSO：`go build -tags integration,sso ./...`
package main

import (
	"context"
	"fmt"

	"email-aggregator-go/src/api"
	"email-aggregator-go/src/auth"
	"email-aggregator-go/src/events"
)

// assembleAuthMiddleware 在非 sso 构建下恒返回 nil。
// 调用方据此跳过 WithAuth 挂载，所有路由按 PoC 开放模式运行。
func assembleAuthMiddleware(_ context.Context, _ events.EventBus) *auth.AuthMiddleware {
	fmt.Println("[auth] 未启用 sso 构建标签，跳过 OIDC 装配（PoC 无鉴权模式）")
	return nil
}

// assembleSSORouter 在非 sso 构建下恒返回 nil（调用方跳过 WithSSO 挂载）。
func assembleSSORouter(_ context.Context, _ events.EventBus) *api.SSORouter {
	return nil
}

// 显式引用 auth 包避免 import 报 unused（若后续未使用）
var _ = auth.NewStaticResolver
