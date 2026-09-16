// SSORouter：PKCE 授权码流的 BFF 侧端点（ADR-009 SSO 落地的最后一段）。
//
// GET /api/auth/login     → 302 到 OIDC Provider（携带 state + code_challenge/S256）
// GET /api/auth/callback  → 302 回跳后换 token、验签 id_token，设会话 cookie 并返回 JSON
//
// 本文件无构建标签：只依赖 stdlib 与 auth.OIDCVerifier 接口（接口本体无标签，
// 具体实现 NewKeycloakVerifier 在 //go:build sso 的 oidc.go 中），
// 因此 `go build ./...` 与 `go build -tags integration,sso ./...` 均可编译。
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"email-aggregator-go/src/auth"
	"email-aggregator-go/src/security"
)

const (
	ssoStateTTL   = 10 * time.Minute // 授权中途停留（切窗口/慢网络）的容忍上限
	ssoCookieName = "ea_session"     // 会话 cookie：value 为一次性 state，HttpOnly + Lax
)

// pendingAuth 一次登录尝试的暂存：state → PKCE verifier（消费即删，防重放）。
type pendingAuth struct {
	pkce      security.PKCE
	createdAt time.Time
}

// SSORouter 承载 /api/auth/* 两条 BFF 端点。无构建标签；verifier 由 cmd 注入。
type SSORouter struct {
	verifier auth.OIDCVerifier
	mu       sync.Mutex
	pending  map[string]pendingAuth
}

// NewSSORouter 构造（verifier 为 nil 时 Mount 不注册任何路由）。
func NewSSORouter(v auth.OIDCVerifier) *SSORouter {
	return &SSORouter{verifier: v, pending: make(map[string]pendingAuth)}
}

// Mount 注册 /api/auth/login 与 /api/auth/callback（须在鉴权 Wrap 之前挂到 mux）。
func (rt *SSORouter) Mount(mux *http.ServeMux) {
	if rt.verifier == nil {
		return
	}
	mux.HandleFunc("/api/auth/login", rt.handleLogin)
	mux.HandleFunc("/api/auth/callback", rt.handleCallback)
}

// handleLogin 生成 state 与 PKCE 对，暂存 verifier 后 302 跳向 IdP 登录页。
func (rt *SSORouter) handleLogin(w http.ResponseWriter, r *http.Request) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "state 生成失败"})
		return
	}
	state := hex.EncodeToString(buf)
	pkce := security.NewPKCE()

	rt.mu.Lock()
	now := time.Now()
	// 顺手清理过期暂存，防止长跑进程下的 map 缓慢增长
	for k, v := range rt.pending {
		if now.Sub(v.createdAt) > ssoStateTTL {
			delete(rt.pending, k)
		}
	}
	rt.pending[state] = pendingAuth{pkce: pkce, createdAt: now}
	rt.mu.Unlock()

	http.Redirect(w, r, rt.verifier.AuthCodeURL(state, pkce), http.StatusFound)
}

// handleCallback 消费一次性 state 换取 token，验签 id_token 后建立会话。
// 成功：200 + {ok, access_token, id_token, id, email, tenant_id, expires_at}
// 失败：400 + {ok:false, error}（state 不匹配/过期、code 换签失败、id_token 验签失败等）
func (rt *SSORouter) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if errParam := q.Get("error"); errParam != "" {
		desc := q.Get("error_description")
		if desc == "" {
			desc = "未知 IdP 错误"
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": fmt.Sprintf("IdP 拒绝：%s（%s）", errParam, desc)})
		return
	}
	code := q.Get("code")
	state := q.Get("state")
	if code == "" || state == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 code 或 state 参数"})
		return
	}

	rt.mu.Lock()
	p, ok := rt.pending[state]
	delete(rt.pending, state) // 一次性消费：防 state/授权码重放
	rt.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "state 无效或已过期，请重新发起登录"})
		return
	}

	tok, err := rt.verifier.Exchange(r.Context(), code, p.pkce)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "授权码换签失败：" + err.Error()})
		return
	}
	if tok == nil || tok.AccessToken == "" || tok.IDToken == "" {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "IdP 未返回完整 token 对"})
		return
	}

	claims, err := rt.verifyIDToken(r.Context(), tok.IDToken)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "id_token 验签失败：" + err.Error()})
		return
	}

	// 会话 cookie：value 用已消费的 state 作一次性会话 ID；寿命对齐 access token 剩余寿命
	maxAge := int(time.Until(tok.ExpiresAt).Seconds())
	if maxAge <= 0 {
		maxAge = 300
	}
	http.SetCookie(w, &http.Cookie{
		Name:     ssoCookieName,
		Value:    state,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"access_token": tok.AccessToken,
		"id_token":     tok.IDToken,
		"id":           claims.Sub,
		"email":        claims.Email,
		"tenant_id":    claims.TenantID,
		"expires_at":   tok.ExpiresAt.Format(time.RFC3339),
	})
}

// verifyIDToken 通过 OIDCVerifier 验签并取 Claims（jwks 拉取与校验在实现内完成）。
func (rt *SSORouter) verifyIDToken(ctx context.Context, rawIDToken string) (*auth.Claims, error) {
	claims, err := rt.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, err
	}
	if claims == nil || claims.Sub == "" {
		return nil, errors.New("id_token 缺少 sub claim")
	}
	return claims, nil
}
