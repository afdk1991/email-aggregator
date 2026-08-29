package aigateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// lastUserContent 取最后一条 user 消息，便于测试服务端回显。
func lastUserContent(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleUser {
			return msgs[i].Content
		}
	}
	if len(msgs) > 0 {
		return msgs[len(msgs)-1].Content
	}
	return ""
}

func TestSelfHostedProvider_Chat(t *testing.T) {
	var got chatCompletionReq
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		auth = r.Header.Get("authorization")
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &got)
		_ = json.NewEncoder(w).Encode(chatCompletionResp{
			Model:   "vllm-7b",
			Choices: []ccChoice{{Message: Message{Role: RoleModel, Content: "内部回复"}}},
			Usage:   usage{PromptTokens: 3, CompletionTokens: 4},
		})
	}))
	defer srv.Close()

	p := NewSelfHostedProvider(srv.URL, "vllm-7b", "")
	resp, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "内部回复" || resp.Model != "vllm-7b" || resp.TokensIn != 3 || resp.TokensOut != 4 {
		t.Fatalf("unexpected resp: %+v", resp)
	}
	if p.Name() != "self-hosted" {
		t.Fatalf("Name()=%s", p.Name())
	}
	// 自托管内部端点：不应携带第三方 API 密钥
	if auth != "" {
		t.Fatalf("self-hosted must not send bearer token, got %q", auth)
	}
	_ = got
}

func TestThirdPartyProvider_SendsAPIKey(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("authorization")
		_ = json.NewEncoder(w).Encode(chatCompletionResp{
			Model:   "gpt-x",
			Choices: []ccChoice{{Message: Message{Role: RoleModel, Content: "外部回复"}}},
		})
	}))
	defer srv.Close()

	p := NewThirdPartyProvider(srv.URL, "gpt-x", "sk-secret")
	if _, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer sk-secret" {
		t.Fatalf("third-party must send bearer auth, got %q", auth)
	}
}

func TestHTTPProvider_Embed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(embeddingsResp{
			Model: "emb",
			Data: []embData{
				{Embedding: []float32{0.1, 0.2}},
				{Embedding: []float32{0.3, 0.4}},
			},
		})
	}))
	defer srv.Close()

	p := NewSelfHostedProvider(srv.URL, "emb", "")
	vecs, err := p.Embed(context.Background(), "t1", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 || len(vecs[0]) != 2 || vecs[0][0] != 0.1 {
		t.Fatalf("unexpected vecs: %v", vecs)
	}
}

// TestRouter_IntegrationRealProviders_RedactionAtBoundary 用真实 httptest 双后端验证：
// 私有租户命中自托管；公共租户命中第三方，且第三方在 HTTP 边界收到的请求体已被脱敏（PII 不出境）。
func TestRouter_IntegrationRealProviders_RedactionAtBoundary(t *testing.T) {
	selfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(chatCompletionResp{
			Model:   "vllm",
			Choices: []ccChoice{{Message: Message{Role: RoleModel, Content: "私有回复"}}},
		})
	}))
	defer selfSrv.Close()

	var thirdPartyGot string
	thirdSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var req chatCompletionReq
		_ = json.Unmarshal(data, &req)
		if len(req.Messages) > 0 {
			thirdPartyGot = req.Messages[len(req.Messages)-1].Content
		}
		_ = json.NewEncoder(w).Encode(chatCompletionResp{
			Model:   "gpt",
			Choices: []ccChoice{{Message: Message{Role: RoleModel, Content: "公共回复"}}},
		})
	}))
	defer thirdSrv.Close()

	selfP := NewSelfHostedProvider(selfSrv.URL, "vllm", "")
	thirdP := NewThirdPartyProvider(thirdSrv.URL, "gpt", "sk")
	r := NewRouter(selfP, thirdP, NewRegexRedactor(), nil)

	// 私有租户 → 自托管
	respPriv, err := r.RouteAndChat(context.Background(), ChatRequest{
		TenantID:    "t-priv",
		TenantTier:  TierPrivate,
		Sensitivity: SensitivityPII,
		Messages:    []Message{{Role: RoleUser, Content: "联系 a@fang.com"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if respPriv.Backend != "self-hosted" {
		t.Fatalf("private must route to self-hosted, got %s", respPriv.Backend)
	}

	// 公共租户 → 第三方，且第三方收到的内容已脱敏
	respPub, err := r.RouteAndChat(context.Background(), ChatRequest{
		TenantID:      "t-pub",
		TenantTier:    TierPublic,
		Sensitivity:   SensitivityPII,
		UserConsented: true,
		Messages:      []Message{{Role: RoleUser, Content: "我的邮箱是 bob@example.com 电话 13912345678"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if respPub.Backend != "third-party" {
		t.Fatalf("public consented PII must route third-party, got %s", respPub.Backend)
	}
	if strings.Contains(thirdPartyGot, "bob@example.com") || strings.Contains(thirdPartyGot, "13912345678") {
		t.Fatalf("PII reached third party unredacted: %q", thirdPartyGot)
	}
	if !strings.Contains(thirdPartyGot, "███") {
		t.Fatalf("expected redaction marker in third-party payload, got %q", thirdPartyGot)
	}
}

func TestHTTPProvider_ClassifyAndSummarize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(chatCompletionResp{
			Model:   "vllm",
			Choices: []ccChoice{{Message: Message{Role: RoleModel, Content: "spam"}}},
		})
	}))
	defer srv.Close()
	p := NewSelfHostedProvider(srv.URL, "vllm", "")

	label, conf, err := p.Classify(context.Background(), "t1", "便宜代开发票", []string{"spam", "ham"})
	if err != nil {
		t.Fatal(err)
	}
	if label != "spam" || conf <= 0 {
		t.Fatalf("classify=%s conf=%v", label, conf)
	}

	summary, err := p.Summarize(context.Background(), ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "很长的内容……"}},
	})
	if err != nil || summary == "" {
		t.Fatalf("summarize err=%v out=%q", err, summary)
	}
}
