// Package auth — AuthMiddleware 网关层鉴权 + RBAC + 审计事件（ADR-009 物理阻断）。
//
// 流程：Bearer 解析 → OIDCVerifier.Verify → Claims 注入 ctx → RBACResolver.Resolve →
// Can 判定 → 403 时 EmitAudit(action=cross-tenant-deny / permission-deny)。
//
// 零外部依赖：本文件纯 stdlib；OIDCVerifier 接口在此定义，oidc.go (//go:build sso) 提供实现。

package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"email-aggregator-go/src/events"
	"email-aggregator-go/src/security"
	"email-aggregator-go/src/tenant"
)

// contextKey Claims 注入 ctx 的键类型，避免冲突。
type contextKey struct{ name string }

// claimsKey 注入 Claims 的 ctx 键。
var claimsKey = contextKey{"auth.claims"}

// Claims OIDC id_token 解析后的用户身份与租户上下文。
// TenantID 从 id_token tenant_id claim 或 groups 推导；Roles 来自 groups claim。
type Claims struct {
	Sub      string   // 用户唯一标识（OIDC sub claim）
	Email    string   // 邮箱
	Name     string   // 显示名
	TenantID string   // 租户 ID（ADR-009）
	Roles    []string // 角色 groups（owner/admin/member/viewer）
}

// FromContext 从 ctx 取 Claims（nil 安全）。
func FromContext(ctx context.Context) (*Claims, bool) {
	c, ok := ctx.Value(claimsKey).(*Claims)
	return c, ok
}

// OIDCVerifier OIDC id_token 验签与 PKCE 交换接口。
// 真实实现（KeycloakVerifier/DexVerifier）在 oidc.go (//go:build sso) 中；
// 测试用 mock 实现可直接嵌入测试文件。
type OIDCVerifier interface {
	// Verify 验签并解析 raw id_token 为 Claims。
	Verify(ctx context.Context, rawIDToken string) (*Claims, error)
	// AuthCodeURL 构造授权跳转 URL（携带 state + PKCE challenge）。
	AuthCodeURL(state string, pkce security.PKCE) string
	// Exchange 用授权码 + PKCE verifier 兑换令牌（含 id_token）。
	Exchange(ctx context.Context, code string, pkce security.PKCE) (*Token, error)
}

// Token OIDC 令牌响应。
type Token struct {
	AccessToken  string
	IDToken      string // 原始 id_token（喂给 Verify）
	RefreshToken string
	ExpiresAt    time.Time
}

// AuthMiddleware 网关层鉴权中间件。
//   - verifier 为 nil 时：中间件无操作（PoC 单租户回退，向后兼容）
//   - verifier 非 nil：所有非 /api/health 路由强制 Bearer 验签
type AuthMiddleware struct {
	verifier OIDCVerifier
	rbac     RBACResolver
	audit    events.EventBus // 可选：nil 时不发审计事件（测试用）
}

// NewAuthMiddleware 构造（verifier 可为 nil 表示禁用鉴权模式）。
func NewAuthMiddleware(verifier OIDCVerifier, rbac RBACResolver, audit events.EventBus) *AuthMiddleware {
	return &AuthMiddleware{verifier: verifier, rbac: rbac, audit: audit}
}

// Wrap 包装 next handler，注入 Bearer 验签 + Claims ctx。
//   - 健康检查 /metrics 端点免鉴权
//   - 缺失/非法 Authorization → 401
//   - 验签失败 → 401
//   - 验签成功但租户头与 JWT tenantId 不一致 → 403 cross-tenant-deny
func (m *AuthMiddleware) Wrap(next http.Handler) http.Handler {
	if m.verifier == nil {
		return next // 禁用鉴权模式（PoC 回退）
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 公开端点免鉴权
		if isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// 提取 Bearer
		raw := bearerToken(r)
		if raw == "" {
			deny(w, r, m, "", "", "missing-bearer", 401)
			return
		}
		// 验签
		claims, err := m.verifier.Verify(r.Context(), raw)
		if err != nil {
			deny(w, r, m, "", "", "invalid-token:"+err.Error(), 401)
			return
		}
		// ADR-009 跨租户阻断：JWT tenantId 必须与 X-Tenant-Id 头一致
		headerTid := tenant.FromRequest(r).TenantID
		if claims.TenantID != "" && headerTid != tenant.DefaultTenantID &&
			claims.TenantID != headerTid {
			deny(w, r, m, headerTid, claims.Sub, "cross-tenant-deny", 403)
			return
		}
		// 注入 Claims 到 ctx
		ctx := context.WithValue(r.Context(), claimsKey, claims)
		// 若头未带 tenantId 但 JWT 有，用 JWT tenantId 回填头（便于下游 tenant.FromRequest 取到正确值）
		if headerTid == tenant.DefaultTenantID && claims.TenantID != "" {
			r.Header.Set(tenant.HeaderTenantID, claims.TenantID)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Require 包装特定动作的权限校验：Can(role, action) → 否则 403 permission-deny。
// 必须在 Wrap 之内生效（即 Wrap 先验签注入 Claims，Require 再判权限）。
func (m *AuthMiddleware) Require(action Action) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 禁用鉴权模式（verifier nil）直接放行
			if m.verifier == nil {
				next.ServeHTTP(w, r)
				return
			}
			claims, ok := FromContext(r.Context())
			if !ok {
				deny(w, r, m, tenant.FromRequest(r).TenantID, "", "no-claims", 401)
				return
			}
			role := m.rbac.Resolve(claims.TenantID, claims.Sub, claims.Roles)
			if !Can(role, action) {
				deny(w, r, m, claims.TenantID, claims.Sub, "permission-deny:"+string(action), 403)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isPublicPath 判定是否免鉴权公开端点（健康检查 / metrics / favicon / SSO 登录流程）。
// /api/auth/login 与 /api/auth/callback 是登录流程本身，登录者尚无 Bearer token，必须放行。
func isPublicPath(path string) bool {
	switch path {
	case "/api/health", "/health", "/api/metrics", "/metrics", "/favicon.ico",
		"/api/auth/login", "/api/auth/callback":
		return true
	}
	return false
}

// bearerToken 从 Authorization 头提取 Bearer token。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(h[len("Bearer "):])
}

// deny 写拒绝响应 + emit AuditEvent。
func deny(w http.ResponseWriter, r *http.Request, m *AuthMiddleware, tid, actor, reason string, status int) {
	body := map[string]any{"error": reason, "tenantId": tid}
	if actor != "" {
		body["actor"] = actor
	}
	// ADR-009 审计：所有 deny 事件强制带 tenantId；actor 空时用 anonymous 占位
	// （安全审计必须记录，身份未知也需溯源标记）
	if m.audit != nil {
		a := actor
		if a == "" {
			a = "anonymous"
		}
		EmitAudit(r.Context(), m.audit, events.AuditEvent{
			TenantID: tenant.Resolve(tid),
			Actor:    a,
			Action:   "auth.deny:" + reason,
			Target:   r.URL.Path,
			At:       time.Now().Unix(),
			Meta: map[string]string{
				"method":     r.Method,
				"remoteAddr": r.RemoteAddr,
				"traceId":    r.Header.Get("X-Trace-ID"),
			},
		})
	}
	writeAuthJSON(w, status, body)
}

// writeAuthJSON 写 JSON 响应（避免 import api 包的 writeJSON 造成循环依赖）。
func writeAuthJSON(w http.ResponseWriter, status int, body any) {
	data, _ := json.Marshal(body)
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
