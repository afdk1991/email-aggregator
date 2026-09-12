package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"email-aggregator-go/src/events"
	"email-aggregator-go/src/security"
	"email-aggregator-go/src/tenant"
)

// mockVerifier 测试用 OIDCVerifier 实现（不依赖 //go:build sso 的 KeycloakVerifier）。
type mockVerifier struct {
	claims *Claims
	err    error
}

func (m *mockVerifier) Verify(_ context.Context, _ string) (*Claims, error) {
	return m.claims, m.err
}
func (m *mockVerifier) AuthCodeURL(_ string, _ security.PKCE) string { return "https://idp/auth" }
func (m *mockVerifier) Exchange(_ context.Context, _ string, _ security.PKCE) (*Token, error) {
	return &Token{IDToken: "mock-id-token"}, nil
}

// captureBus 记录 Publish 事件供断言。
type captureBus struct {
	published []events.AuditEvent
}

func (b *captureBus) Publish(_ context.Context, _ string, _ string, payload []byte) error {
	var ev events.AuditEvent
	_ = json.Unmarshal(payload, &ev)
	b.published = append(b.published, ev)
	return nil
}
func (b *captureBus) Subscribe(context.Context, string, string, func(context.Context, events.EventEnvelope) error) error {
	return nil
}
func (b *captureBus) Close() error { return nil }

func TestWrap_PublicEndpointNoAuth(t *testing.T) {
	m := NewAuthMiddleware(&mockVerifier{claims: &Claims{Sub: "u1", TenantID: "t1"}}, NewStaticResolver(), &captureBus{})
	called := false
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(200)
	}))
	req := httptest.NewRequest("GET", "/api/health", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !called {
		t.Fatal("public path /api/health should bypass auth")
	}
}

func TestWrap_NoBearer_401(t *testing.T) {
	bus := &captureBus{}
	m := NewAuthMiddleware(&mockVerifier{claims: &Claims{Sub: "u1"}}, NewStaticResolver(), bus)
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next should not be called")
	}))
	req := httptest.NewRequest("GET", "/api/mails", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("no bearer: got %d want 401", rec.Code)
	}
	if len(bus.published) != 1 || bus.published[0].Action != "auth.deny:missing-bearer" {
		t.Errorf("audit: got %v", bus.published)
	}
}

func TestWrap_ValidBearer_CtxClaims(t *testing.T) {
	m := NewAuthMiddleware(&mockVerifier{claims: &Claims{Sub: "u1", TenantID: "t1"}}, NewStaticResolver(), &captureBus{})
	var got *Claims
	h := m.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = FromContext(r.Context())
	}))
	req := httptest.NewRequest("GET", "/api/mails", nil)
	req.Header.Set("Authorization", "Bearer fake-id-token")
	req.Header.Set(tenant.HeaderTenantID, "t1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got == nil || got.Sub != "u1" {
		t.Fatalf("claims not injected: %+v", got)
	}
}

func TestWrap_CrossTenantDeny_403(t *testing.T) {
	bus := &captureBus{}
	m := NewAuthVerifier(&mockVerifier{claims: &Claims{Sub: "u1", TenantID: "t-jwt"}}, NewStaticResolver(), bus)
	h := m.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("cross-tenant should not reach next")
	}))
	req := httptest.NewRequest("GET", "/api/mails", nil)
	req.Header.Set("Authorization", "Bearer fake")
	req.Header.Set(tenant.HeaderTenantID, "t-other") // 头与 JWT 不一致
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("cross-tenant: got %d want 403", rec.Code)
	}
	if len(bus.published) != 1 || bus.published[0].Action != "auth.deny:cross-tenant-deny" {
		t.Errorf("audit: %v", bus.published)
	}
}

// 修正：方法名笔误 NewAuthVerifier -> NewAuthMiddleware
func NewAuthVerifier(v OIDCVerifier, r RBACResolver, b events.EventBus) *AuthMiddleware {
	return NewAuthMiddleware(v, r, b)
}

func TestRequire_PermissionDeny_403(t *testing.T) {
	bus := &captureBus{}
	m := NewAuthMiddleware(&mockVerifier{claims: &Claims{Sub: "u1", TenantID: "t1", Roles: []string{"viewer"}}}, NewStaticResolver(), bus)
	// 先 Wrap 注入 claims，再 Require 判权限
	h := m.Wrap(m.Require(ActionAccountManage)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("viewer should not pass account.manage")
	})))
	req := httptest.NewRequest("POST", "/api/accounts", nil)
	req.Header.Set("Authorization", "Bearer fake")
	req.Header.Set(tenant.HeaderTenantID, "t1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("viewer: got %d want 403", rec.Code)
	}
	if len(bus.published) != 1 || bus.published[0].Action != "auth.deny:permission-deny:account.manage" {
		t.Errorf("audit: %v", bus.published)
	}
}

func TestRequire_OwnerPass(t *testing.T) {
	bus := &captureBus{}
	m := NewAuthMiddleware(&mockVerifier{claims: &Claims{Sub: "u1", TenantID: "t1", Roles: []string{"owner"}}}, NewStaticResolver(), bus)
	called := false
	h := m.Wrap(m.Require(ActionAccountCreate)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	})))
	req := httptest.NewRequest("POST", "/api/accounts", nil)
	req.Header.Set("Authorization", "Bearer fake")
	req.Header.Set(tenant.HeaderTenantID, "t1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !called {
		t.Fatal("owner should pass account.create")
	}
}

func TestEmitAudit_EmptyActorRejected(t *testing.T) {
	bus := &captureBus{}
	err := EmitAudit(context.Background(), bus, events.AuditEvent{
		TenantID: "t1",
		Actor:    "", // 空 Actor 必须拒绝
		Action:   "test",
	})
	if err == nil {
		t.Fatal("empty actor must be rejected")
	}
}

func TestEmitAudit_TenantResolve(t *testing.T) {
	bus := &captureBus{}
	err := EmitAudit(context.Background(), bus, events.AuditEvent{
		TenantID: "", // 空值应回退 DefaultTenantID
		Actor:    "u1",
		Action:   "test",
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if len(bus.published) != 1 {
		t.Fatal("event not published")
	}
	if bus.published[0].TenantID != tenant.DefaultTenantID {
		t.Errorf("tenantId: got %q want %q", bus.published[0].TenantID, tenant.DefaultTenantID)
	}
}

func TestWrap_DisabledMode_NoVerifier(t *testing.T) {
	m := NewAuthMiddleware(nil, nil, nil) // verifier=nil 禁用模式
	called := false
	h := m.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	req := httptest.NewRequest("GET", "/api/mails", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !called {
		t.Fatal("disabled mode should pass through")
	}
}
