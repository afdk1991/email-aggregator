// Package auth — OIDC SSO 验签器（//go:build sso）。
//
// 引入 github.com/coreos/go-oidc/v3 + golang.org/x/oauth2 实现 Keycloak/Dex OIDC 验签。
// 默认 `go build ./...` 不编译本文件，保持 PoC 零外部依赖能力。
// `go build -tags sso ./...` 启用 SSO 装配。

//go:build sso

package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	oidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"email-aggregator-go/src/security"
)

// KeycloakVerifier Keycloak / Dex / Auth0 等 OIDC IdP 验签器。
type KeycloakVerifier struct {
	provider    *oidc.Provider
	verifier    *oidc.IDTokenVerifier
	oauthConfig *oauth2.Config
	clientID    string
}

// NewKeycloakVerifier 构造：issuer 如 "http://keycloak:8080/realms/email-aggregator"，
// clientID/secret 来自环境变量（client_secret 走 vault.Unseal 解封后注入）。
func NewKeycloakVerifier(ctx context.Context, issuer, clientID, clientSecret, redirectURI string) (*KeycloakVerifier, error) {
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: new provider %s: %w", issuer, err)
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: clientID})
	oauthConfig := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURI,
		Endpoint:     provider.Endpoint(),
		Scopes:       []string{"openid", "profile", "email", "groups"},
	}
	return &KeycloakVerifier{provider: provider, verifier: verifier, oauthConfig: oauthConfig, clientID: clientID}, nil
}

// Verify 验签 raw id_token 并提取 Claims（Sub/Email/Name/TenantID/Roles）。
//   - TenantID 从 id_token tenant_id claim 或 groups 中 tenant:<tid> 推导
//   - Roles 从 groups claim 取（owner/admin/member/viewer）
func (v *KeycloakVerifier) Verify(ctx context.Context, rawIDToken string) (*Claims, error) {
	tok, err := v.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("oidc: verify: %w", err)
	}
	// 提取 claims
	var raw struct {
		Sub      string   `json:"sub"`
		Email    string   `json:"email"`
		Name     string   `json:"name"`
		TenantID string   `json:"tenant_id"` // 自定义 claim（Keycloak mapper 配置）
		Groups   []string `json:"groups"`
	}
	if err := tok.Claims(&raw); err != nil {
		return nil, fmt.Errorf("oidc: claims: %w", err)
	}
	// TenantID 兜底：若 id_token 无 tenant_id，从 groups 中 tenant:<tid> 推导
	tid := raw.TenantID
	if tid == "" {
		for _, g := range raw.Groups {
			if len(g) > 7 && g[:7] == "tenant:" {
				tid = g[7:]
				break
			}
		}
	}
	return &Claims{
		Sub:      raw.Sub,
		Email:    raw.Email,
		Name:     raw.Name,
		TenantID: tid,
		Roles:    raw.Groups,
	}, nil
}

// AuthCodeURL 构造授权跳转 URL（携带 state + PKCE challenge）。
func (v *KeycloakVerifier) AuthCodeURL(state string, pkce security.PKCE) string {
	// oauth2 库的 AuthCodeURL 原生支持 PKCE（code_challenge + code_challenge_method=S256）
	// 但需要通过 AuthCodeURL 的额外参数传入；这里复用 security.PKCE 的 Challenge
	return v.oauthConfig.AuthCodeURL(state,
		oauth2.SetAuthURLParam("code_challenge", pkce.Challenge),
		oauth2.SetAuthURLParam("code_challenge_method", pkce.Method),
	)
}

// Exchange 用授权码 + PKCE verifier 兑换令牌（含 id_token）。
func (v *KeycloakVerifier) Exchange(ctx context.Context, code string, pkce security.PKCE) (*Token, error) {
	tok, err := v.oauthConfig.Exchange(ctx, code,
		oauth2.SetAuthURLParam("code_verifier", pkce.Verifier),
	)
	if err != nil {
		return nil, fmt.Errorf("oidc: exchange: %w", err)
	}
	// 提取 id_token（OIDC scope 必返回）
	rawID, ok := tok.Extra("id_token").(string)
	if !ok {
		return nil, fmt.Errorf("oidc: no id_token in token response")
	}
	return &Token{
		AccessToken:  tok.AccessToken,
		IDToken:      rawID,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    tok.Expiry,
	}, nil
}

// _ 保证 time/json 包被引用（避免 go vet 未使用告警）
var _ = time.Now
var _ = json.Marshal
