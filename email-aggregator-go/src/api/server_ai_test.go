package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"email-aggregator-go/src/aigateway"
	"email-aggregator-go/src/notify"
	"email-aggregator-go/src/search"
	"email-aggregator-go/src/store"
)

// postAIChat 向 /api/ai/chat 发请求并返回状态码与解析后的响应体。
func postAIChat(t *testing.T, h http.Handler, body aigateway.ChatRequest) (int, *aigateway.ChatResponse) {
	t.Helper()
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/ai/chat", bytes.NewReader(data))
	req = req.WithContext(context.Background())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp aigateway.ChatResponse
	if rec.Code == 200 {
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	}
	return rec.Code, &resp
}

func newGatewayServer() http.Handler {
	self := &aigateway.StubProvider{Name_: "vllm", Backend: "self-hosted"}
	third := &aigateway.StubProvider{Name_: "claude", Backend: "third-party"}
	r := aigateway.NewRouter(self, third, aigateway.NewRegexRedactor(), nil)
	srv := NewApiServer(store.NewInMemoryMetadataStore(), search.NewInMemorySearchIndex(), notify.NewInMemoryNotifier(), 0)
	return srv.WithAIGateway(r).Handler()
}

func TestAIChat_PrivateRoutesSelfHosted(t *testing.T) {
	h := newGatewayServer()
	code, resp := postAIChat(t, h, aigateway.ChatRequest{
		TenantID:    "t-priv",
		TenantTier:  aigateway.TierPrivate,
		Sensitivity: aigateway.SensitivityPII,
		Messages:    []aigateway.Message{{Role: aigateway.RoleUser, Content: "联系 a@fang.com"}},
	})
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if resp.Backend != "self-hosted" {
		t.Fatalf("private must route self-hosted, got %s", resp.Backend)
	}
}

func TestAIChat_PublicPIIRedactedThirdParty(t *testing.T) {
	h := newGatewayServer()
	code, resp := postAIChat(t, h, aigateway.ChatRequest{
		TenantID:      "t-pub",
		TenantTier:    aigateway.TierPublic,
		Sensitivity:   aigateway.SensitivityPII,
		UserConsented: true,
		Messages:      []aigateway.Message{{Role: aigateway.RoleUser, Content: "邮箱 bob@example.com 电话 13912345678"}},
	})
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if resp.Backend != "third-party" || !resp.Redacted {
		t.Fatalf("expected third-party + redacted, got backend=%s redacted=%v", resp.Backend, resp.Redacted)
	}
	if strings.Contains(resp.Content, "bob@example.com") {
		t.Fatalf("PII leaked into response: %s", resp.Content)
	}
}

func TestAIChat_PublicPIINoConsentDenied(t *testing.T) {
	h := newGatewayServer()
	code, _ := postAIChat(t, h, aigateway.ChatRequest{
		TenantID:      "t-pub",
		TenantTier:    aigateway.TierPublic,
		Sensitivity:   aigateway.SensitivityPII,
		UserConsented: false,
		Messages:      []aigateway.Message{{Role: aigateway.RoleUser, Content: "有 PII"}},
	})
	if code != 422 {
		t.Fatalf("expected 422 denial, got %d", code)
	}
}
