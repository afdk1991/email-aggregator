//go:build integration && sso && !sync_worker && !ingest_worker && !search_service

// Phase 3 / ADR-009：OIDC SSO 装配（sso 标签生效）。
//
// 在 integration + sso 双标签下编译。读取 OIDC_* 环境变量构造 KeycloakVerifier，
// 配合 StaticResolver + EventBus 组装 AuthMiddleware。未配置 OIDC_ISSUER 时返回 nil，
// 服务器回退至 PoC 无鉴权模式（向后兼容）。
package main

import (
	"context"
	"fmt"
	"os"

	"email-aggregator-go/src/auth"
	"email-aggregator-go/src/events"
)

// assembleAuthMiddleware 根据 OIDC_* 环境变量装配 AuthMiddleware。
//
// 必填环境变量：
//   - OIDC_ISSUER     如 "http://keycloak:8080/realms/email-aggregator"
//   - OIDC_CLIENT_ID
//   - OIDC_CLIENT_SECRET（可经 vault 解封后注入）
//   - OIDC_REDIRECT_URI 如 "http://localhost:8080/api/auth/callback"
//
// 返回 nil 表示未配置 OIDC，调用方应回退至无鉴权模式（不挂载 WithAuth）。
func assembleAuthMiddleware(_ context.Context, bus events.EventBus) *auth.AuthMiddleware {
	issuer := os.Getenv("OIDC_ISSUER")
	clientID := os.Getenv("OIDC_CLIENT_ID")
	if issuer == "" || clientID == "" {
		fmt.Println("[auth] OIDC_ISSUER/CLIENT_ID 未配置，跳过 SSO 装配（PoC 无鉴权模式）")
		return nil
	}
	verifier, err := auth.NewKeycloakVerifier(
		context.Background(),
		issuer,
		clientID,
		os.Getenv("OIDC_CLIENT_SECRET"),
		env("OIDC_REDIRECT_URI", "http://localhost:8080/api/auth/callback"),
	)
	if err != nil {
		// OIDC IdP 不可达时打印告警并回退至无鉴权模式：避免 IdP 故障导致服务完全不可用。
		// 生产环境建议通过 readiness probe 把 IdP 不可达视为致命故障。
		fmt.Printf("[warn] OIDC verifier 构造失败，回退至无鉴权模式: %v\n", err)
		return nil
	}
	fmt.Printf("[auth] OIDC SSO 已启用，issuer=%s clientID=%s\n", issuer, clientID)
	return auth.NewAuthMiddleware(verifier, auth.NewStaticResolver(), bus)
}
