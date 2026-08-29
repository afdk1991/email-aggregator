package aigateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPProviderConfig 配置一个 OpenAI 兼容的后端提供方。
// 自托管（vLLM 等）与第三方 API 共用同一 HTTP 契约，仅 BaseURL/APIKey/Kind 不同。
type HTTPProviderConfig struct {
	Kind    string // "self-hosted" | "third-party"（仅用于 Name() 与可观测）
	BaseURL string // OpenAI 兼容端点，如 http://vllm.internal/v1 或 https://api.thirdparty.com/v1
	Model   string
	APIKey  string       // 内部自托管可为空；第三方必填
	Client  *http.Client // 可选注入（测试用 httptest）
}

// HTTPProvider 真实 HTTP 后端（OpenAI 兼容 /v1/chat/completions 与 /v1/embeddings）。
// 仅依赖标准库；默认构建即可编译，可用 httptest 单测验证（无需外网）。
// 数据出境策略由 Router 决定（ADR-010）：私有/企业强制 self-hosted；公共经同意+脱敏走 third-party。
type HTTPProvider struct {
	cfg HTTPProviderConfig
}

// NewSelfHostedProvider 构造自托管提供方（私有/企业租户强制零出境路径）。
// baseURL 指向部署在租户 VPC 内的 vLLM/兼容端点；apiKey 可空（内网互信）。
func NewSelfHostedProvider(baseURL, model, apiKey string) *HTTPProvider {
	return &HTTPProvider{cfg: HTTPProviderConfig{Kind: "self-hosted", BaseURL: baseURL, Model: model, APIKey: apiKey}}
}

// NewThirdPartyProvider 构造第三方 API 提供方（公共租户显式同意+脱敏后路径）。
func NewThirdPartyProvider(baseURL, model, apiKey string) *HTTPProvider {
	return &HTTPProvider{cfg: HTTPProviderConfig{Kind: "third-party", BaseURL: baseURL, Model: model, APIKey: apiKey}}
}

func (p *HTTPProvider) httpClient() *http.Client {
	if p.cfg.Client != nil {
		return p.cfg.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Name 实现 AIProvider。
func (p *HTTPProvider) Name() string { return p.cfg.Kind }

// ---- OpenAI 兼容契约（请求/响应类型，命名以便单测复用） ----

type chatCompletionReq struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

type ccChoice struct {
	Message Message `json:"message"`
}

type chatCompletionResp struct {
	Model   string     `json:"model"`
	Choices []ccChoice `json:"choices"`
	Usage   usage      `json:"usage"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type embeddingsReq struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embData struct {
	Embedding []float32 `json:"embedding"`
}

type embeddingsResp struct {
	Model string    `json:"model"`
	Data  []embData `json:"data"`
}

// doJSON 发 JSON POST 并读取响应体，统一错误处理（含上游非 2xx）。
func (p *HTTPProvider) doJSON(ctx context.Context, method, url string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	if p.cfg.APIKey != "" {
		req.Header.Set("authorization", "Bearer "+p.cfg.APIKey)
	}
	resp, err := p.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("aigateway: upstream %s %d: %s", url, resp.StatusCode, string(data))
	}
	return data, nil
}

// Chat 实现 AIProvider：调用 /v1/chat/completions。
func (p *HTTPProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	body, err := json.Marshal(chatCompletionReq{Model: p.cfg.Model, Messages: req.Messages, Stream: false})
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(p.cfg.BaseURL, "/") + "/chat/completions"
	out, err := p.doJSON(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	var cr chatCompletionResp
	if err := json.Unmarshal(out, &cr); err != nil {
		return nil, err
	}
	if len(cr.Choices) == 0 {
		return nil, errors.New("aigateway: empty chat choices from upstream")
	}
	return &ChatResponse{
		Content:   cr.Choices[0].Message.Content,
		Model:     cr.Model,
		TokensIn:  cr.Usage.PromptTokens,
		TokensOut: cr.Usage.CompletionTokens,
	}, nil
}

// Embed 实现 AIProvider：调用 /v1/embeddings。
func (p *HTTPProvider) Embed(ctx context.Context, _ string, texts []string) ([][]float32, error) {
	body, err := json.Marshal(embeddingsReq{Model: p.cfg.Model, Input: texts})
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(p.cfg.BaseURL, "/") + "/embeddings"
	out, err := p.doJSON(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	var er embeddingsResp
	if err := json.Unmarshal(out, &er); err != nil {
		return nil, err
	}
	vecs := make([][]float32, len(er.Data))
	for i, d := range er.Data {
		vecs[i] = d.Embedding
	}
	return vecs, nil
}

// Classify 实现 AIProvider：以分类提示词经 Chat 实现（复用同一后端）。
func (p *HTTPProvider) Classify(ctx context.Context, tenantID string, text string, labels []string) (string, float64, error) {
	if len(labels) == 0 {
		return "", 0, errors.New("aigateway: no labels provided")
	}
	prompt := fmt.Sprintf("将文本分类为下列标签之一，仅回复标签名（不要解释）：%v\n文本：%s", labels, text)
	resp, err := p.Chat(ctx, ChatRequest{
		TenantID:   tenantID,
		Capability: CapClassify,
		Messages: []Message{
			{Role: RoleSystem, Content: "你是一个文本分类器。"},
			{Role: RoleUser, Content: prompt},
		},
	})
	if err != nil {
		return "", 0, err
	}
	return strings.TrimSpace(resp.Content), 0.9, nil
}

// Summarize 实现 AIProvider：以摘要提示词经 Chat 实现。
func (p *HTTPProvider) Summarize(ctx context.Context, req ChatRequest) (string, error) {
	resp, err := p.Chat(ctx, ChatRequest{
		TenantID:      req.TenantID,
		TenantTier:    req.TenantTier,
		AccountID:     req.AccountID,
		Capability:    CapSummarize,
		Sensitivity:   req.Sensitivity,
		UserConsented: req.UserConsented,
		Messages: append([]Message{
			{Role: RoleSystem, Content: "请用简洁中文总结以下内容，不超过 3 句话。"},
		}, req.Messages...),
	})
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}
