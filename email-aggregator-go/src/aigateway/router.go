package aigateway

import (
	"context"
	"errors"
	"regexp"
)

// RouteDecision 路由决策结果。
type RouteDecision struct {
	Provider         string
	RequireRedaction bool
	Allowed          bool
	Reason           string
}

// Decide 纯函数策略：按 (租户类别, 敏感度, 是否同意) 选路。
// 约束（ADR-009/010）：
//   - 私有/企业租户：强制自托管，数据零出境；即便含 PII 也不出平台。
//   - 公共租户：含 PII 且未同意 → 拒绝出第三方；含 PII 且同意 → 第三方 + 强制脱敏；无 PII → 第三方。
func Decide(tier TenantTier, sensitivity DataSensitivity, consented bool) RouteDecision {
	switch tier {
	case TierPrivate, TierEnterprise:
		return RouteDecision{
			Provider:         "self-hosted",
			RequireRedaction: false,
			Allowed:          true,
			Reason:           "私有/企业租户强制自托管，数据零出境",
		}
	case TierPublic:
		if sensitivity == SensitivityPII && !consented {
			return RouteDecision{
				Provider:         "",
				RequireRedaction: false,
				Allowed:          false,
				Reason:           "公共租户含 PII 但未获显式同意，拒绝出第三方（可降级自托管）",
			}
		}
		return RouteDecision{
			Provider:         "third-party",
			RequireRedaction: sensitivity == SensitivityPII,
			Allowed:          true,
			Reason:           "公共租户走第三方 API",
		}
	default:
		return RouteDecision{Allowed: false, Reason: "未知租户类别"}
	}
}

// Router 能力路由网关。
type Router struct {
	providers map[string]AIProvider
	redactor  Redactor
	auditSink func(ctx context.Context, ev AuditEvent)
}

// AuditEvent AI 调用审计事件（入 audit 主题，继承 ADR-002）。
type AuditEvent struct {
	TenantID   string `json:"tenantId"`
	AccountID  string `json:"accountId,omitempty"`
	Capability string `json:"capability"`
	Backend    string `json:"backend"`
	Redacted   bool   `json:"redacted"`
	Allowed    bool   `json:"allowed"`
	Reason     string `json:"reason"`
}

// NewRouter 装配网关。selfHosted/thirdParty 可为 nil（仅装配实际接入的后端）。
func NewRouter(selfHosted, thirdParty AIProvider, redactor Redactor, auditSink func(ctx context.Context, ev AuditEvent)) *Router {
	m := map[string]AIProvider{}
	if selfHosted != nil {
		m["self-hosted"] = selfHosted
	}
	if thirdParty != nil {
		m["third-party"] = thirdParty
	}
	return &Router{providers: m, redactor: redactor, auditSink: auditSink}
}

// RouteAndChat 执行 路由 + Guardrail + 审计，返回结果（失败由调用方接 DLQ/退避）。
func (r *Router) RouteAndChat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	dec := Decide(req.TenantTier, req.Sensitivity, req.UserConsented)

	if r.auditSink != nil {
		r.auditSink(ctx, AuditEvent{
			TenantID:   req.TenantID,
			AccountID:  req.AccountID,
			Capability: string(req.Capability),
			Backend:    dec.Provider,
			Redacted:   dec.RequireRedaction,
			Allowed:    dec.Allowed,
			Reason:     dec.Reason,
		})
	}

	if !dec.Allowed {
		return nil, errors.New("aigateway: route denied: " + dec.Reason)
	}

	provider, ok := r.providers[dec.Provider]
	if !ok {
		return nil, errors.New("aigateway: provider not wired: " + dec.Provider)
	}

	// Guardrail：出平台前脱敏（仅第三方路径触发）。
	effective := req
	if dec.RequireRedaction && r.redactor != nil {
		for i := range effective.Messages {
			cleaned, _ := r.redactor.Redact(effective.Messages[i].Content)
			effective.Messages[i].Content = cleaned
		}
	}

	resp, err := provider.Chat(ctx, effective)
	if err != nil {
		return nil, err
	}
	resp.Redacted = dec.RequireRedaction
	resp.Backend = dec.Provider
	return resp, nil
}

// Redactor PII 脱敏接口。
type Redactor interface {
	Redact(text string) (string, []string)
}

// RegexRedactor 基于正则的最小化脱敏（邮箱/手机/银行卡/身份证）。
type RegexRedactor struct {
	patterns []*regexp.Regexp
}

// NewRegexRedactor 构造默认脱敏器。
func NewRegexRedactor() *RegexRedactor {
	return &RegexRedactor{patterns: []*regexp.Regexp{
		regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`), // email
		regexp.MustCompile(`(?:\+?86)?1[3-9]\d{9}`),                            // CN mobile
		regexp.MustCompile(`\b\d{13,19}\b`),                                    // card / bank
		regexp.MustCompile(`\b\d{17}[\dXx]\b`),                                 // ID card
	}}
}

// Redact 将命中模式的 PII 替换并回传命中列表（供审计/可观测）。
func (rr *RegexRedactor) Redact(text string) (string, []string) {
	var found []string
	out := text
	for _, p := range rr.patterns {
		matches := p.FindAllString(out, -1)
		found = append(found, matches...)
		out = p.ReplaceAllString(out, "███")
	}
	return out, found
}

// StubProvider 测试/默认桩：按名字回声，便于 demo 与单测；真实后端在 integration 标签下实现。
type StubProvider struct {
	Name_   string
	Model_  string
	Backend string
}

func (s *StubProvider) Name() string { return s.Name_ }

func (s *StubProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	last := ""
	if len(req.Messages) > 0 {
		last = req.Messages[len(req.Messages)-1].Content
	}
	backend := s.Backend
	if backend == "" {
		backend = s.Name_
	}
	return &ChatResponse{
		Content:   "[stub:" + backend + "] " + last,
		Backend:   backend,
		Model:     s.Model_,
		TokensIn:  len(last),
		TokensOut: len(last),
	}, nil
}

func (s *StubProvider) Embed(ctx context.Context, tenantID string, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{float32(len(texts[i]) % 7)} // 确定性哑向量
	}
	return out, nil
}

func (s *StubProvider) Classify(ctx context.Context, tenantID string, text string, labels []string) (string, float64, error) {
	if len(labels) == 0 {
		return "", 0, errors.New("no labels")
	}
	return labels[0], 0.9, nil
}

func (s *StubProvider) Summarize(ctx context.Context, req ChatRequest) (string, error) {
	resp, err := s.Chat(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}
