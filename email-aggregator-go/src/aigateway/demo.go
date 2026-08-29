package aigateway

import (
	"context"
	"errors"
	"fmt"
)

// LocalDemoProvider 无外网依赖的本地回环 AIProvider。
// 仅在未配置真实 LLM 端点（AI_THIRD_PARTY_URL / AI_SELF_HOSTED_URL 为占位或空）时由 cmd 装配启用，
// 使 /api/ai/chat 开箱返回 200 且 AI 网关全链路（路由/脱敏/审计）可端到端验证；
// 一旦配置真实端点，即由 HTTPProvider 接管真实 LLM 调用。
// 响应内容显式标注 [demo]，避免与真实模型输出混淆（不伪造真实 LLM 结果）。
type LocalDemoProvider struct {
	Backend string // 固定为 "demo"
	Model   string
}

// NewLocalDemoProvider 构造本地 demo 回环提供方。
func NewLocalDemoProvider() *LocalDemoProvider {
	return &LocalDemoProvider{Backend: "demo", Model: "demo-echo"}
}

// Name 实现 AIProvider。
func (p *LocalDemoProvider) Name() string { return p.Backend }

// Chat 实现 AIProvider：回显最后一条消息并携带租户上下文，便于联调观测网关透传。
func (p *LocalDemoProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	last := ""
	if len(req.Messages) > 0 {
		last = req.Messages[len(req.Messages)-1].Content
	}
	content := fmt.Sprintf("[demo:%s] tenant=%s tier=%s consented=%v\n你刚才说：%s",
		p.Backend, req.TenantID, req.TenantTier, req.UserConsented, last)
	return &ChatResponse{
		Content:   content,
		Backend:   p.Backend,
		Model:     p.Model,
		TokensIn:  len(last),
		TokensOut: len(content),
	}, nil
}

// Embed 实现 AIProvider：返回确定性哑向量（与 StubProvider 同语义）。
func (p *LocalDemoProvider) Embed(ctx context.Context, _ string, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{float32(len(texts[i]) % 7)}
	}
	return out, nil
}

// Classify 实现 AIProvider。
func (p *LocalDemoProvider) Classify(ctx context.Context, _ string, _ string, labels []string) (string, float64, error) {
	if len(labels) == 0 {
		return "", 0, errors.New("no labels")
	}
	return labels[0], 0.9, nil
}

// Summarize 实现 AIProvider：复用 Chat 生成摘要。
func (p *LocalDemoProvider) Summarize(ctx context.Context, req ChatRequest) (string, error) {
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}
