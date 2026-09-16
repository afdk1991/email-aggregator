//go:build integration && sso && !sync_worker && !ingest_worker && !search_service

// Phase 3 / ADR-009：OIDC SSO 装配（sso 标签生效）。
//
// 在 integration + sso 双标签下编译。读取 OIDC_* 环境变量构造 KeycloakVerifier，
// 配合 StaticResolver + EventBus 组装 AuthMiddleware，并为 /api/auth/login|callback
// 提供 SSORouter（PKCE 授权码流 BFF 端点）。未配置 OIDC_ISSUER 时均返回 nil，
// 服务器回退至 PoC 无鉴权模式（向后兼容）。
package main

import (
	"context"
	"fmt"
	"os"
	"sync"

	"email-aggregator-go/src/api"
	"email-aggregator-go/src/auth"
	"email-aggregator-go/src/events"
)

// oidcConfig 中间件与 SSO 端点共享的装配结果（构造含 Discovery + JWKS 引导，仅做一次）。
type oidcConfig struct {
	verifier auth.OIDCVerifier
	redirect string
}

var (
	oidcCfgOnce sync.Once
	oidcCfg     *oidcConfig
)

func oidcConfigCached() *oidcConfig {
	oidcCfgOnce.Do(func() {
		oidcCfg = newOIDCConfig()
	})
	return oidcCfg
}

// newOIDCConfig 读取 OIDC_* 环境变量构造 verifier。
//
// 必填环境变量：
//   - OIDC_ISSUER     如 "http://keycloak:8080/realms/email-aggregator"
//   - OIDC_CLIENT_ID
//   - OIDC_CLIENT_SECRET（可经 vault 解封后注入）
//   - OIDC_REDIRECT_URI 如 "http://localhost:8080/api/auth/callback"
//
// 返回 nil 表示未配置 OIDC（或 IdP 不可达），调用方回退至无鉴权模式。
func newOIDCConfig() *oidcConfig {
	issuer := os.Getenv("OIDC_ISSUER")
	clientID := os.Getenv("OIDC_CLIENT_ID")
	if issuer == "" || clientID == "" {
		fmt.Println("[auth] OIDC_ISSUER/CLIENT_ID 未配置，跳过 SSO 装配（PoC 无鉴权模式）")
		return nil
	}
	redirect := env("OIDC_REDIRECT_URI", "http://localhost:8080/api/auth/callback")
	verifier, err := auth.NewKeycloakVerifier(
		context.Background(),
		issuer,
		clientID,
		os.Getenv("OIDC_CLIENT_SECRET"),
		redirect,
	)
	if err != nil {
		// OIDC IdP 不可达时打印告警并回退至无鉴权模式：避免 IdP 故障导致服务完全不可用。
		// 生产环境建议通过 readiness probe 把 IdP 不可达视为致命故障。
		fmt.Printf("[warn] OIDC verifier 构造失败，回退至无鉴权模式: %v\n", err)
		return nil
	}
	fmt.Printf("[auth] OIDC SSO 已启用，issuer=%s clientID=%s\n", issuer, clientID)
	return &oidcConfig{verifier: verifier, redirect: redirect}
}

// assembleAuthMiddleware 根据 OIDC_* 环境变量装配 AuthMiddleware（签名与无 sso 构建一致）。
// 返回 nil 表示未配置 OIDC，调用方应回退至无鉴权模式（不挂载 WithAuth）。
func assembleAuthMiddleware(_ context.Context, bus events.EventBus) *auth.AuthMiddleware {
	cfg := oidcConfigCached()
	if cfg == nil {
		return nil
	}
	return auth.NewAuthMiddleware(cfg.verifier, auth.NewStaticResolver(), bus)
}

// assembleSSORouter 装配 PKCE 授权码流 BFF 端点（login 302 + callback 换签建会话）。
// 未配置 OIDC 时返回 nil，调用方跳过 WithSSO 挂载。
func assembleSSORouter(_ context.Context, _ events.EventBus) *api.SSORouter {
	cfg := oidcConfigCached()
	if cfg == nil {
		return nil
	}
	return api.NewSSORouter(cfg.verifier)
}
