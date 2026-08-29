// Package aigateway 实现 ADR-010 的「能力路由网关」抽象：
// 统一 AIProvider 接口 + 按租户类别/数据敏感度/用户同意选路的 Router + Guardrail 脱敏层。
// 仅依赖标准库，零外部依赖；与 events/store/search/notify 同构（ADR-002/003/005）。
package aigateway

import "context"

// Capability 标识 AI 子能力（对应 ADR-010 的 Chat/Embed/Classify/Summarize/Extract）。
type Capability string

const (
	CapChat      Capability = "chat"
	CapEmbed     Capability = "embed"
	CapClassify  Capability = "classify"
	CapSummarize Capability = "summarize"
	CapExtract   Capability = "extract"
)

// TenantTier 与 ADR-009 的部署形态对齐。
type TenantTier string

const (
	TierPrivate    TenantTier = "private"    // 私有化：数据零出境
	TierEnterprise TenantTier = "enterprise" // 企业专属栈
	TierPublic     TenantTier = "public"     // 公共 SaaS 共享栈
)

// DataSensitivity 数据敏感度分级。
type DataSensitivity int

const (
	SensitivityClean DataSensitivity = 0 // 无 PII
	SensitivityPII   DataSensitivity = 1 // 含 PII（地址/电话/卡号/身份证等）
)

// Role 消息角色。
type Role string

const (
	RoleSystem Role = "system"
	RoleUser   Role = "user"
	RoleModel  Role = "model"
)

// Message 单轮对话消息。
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

// ChatRequest 统一请求契约（全链路携带租户上下文，继承 ADR-009）。
type ChatRequest struct {
	TenantID      string          `json:"tenantId"`
	TenantTier    TenantTier      `json:"tenantTier"`
	AccountID     string          `json:"accountId,omitempty"`
	Capability    Capability      `json:"capability"`
	Messages      []Message       `json:"messages"`
	Sensitivity   DataSensitivity `json:"sensitivity"`
	UserConsented bool            `json:"userConsented"` // 仅公共租户走第三方时生效
}

// ChatResponse 统一响应契约。
type ChatResponse struct {
	Content   string `json:"content"`
	Backend   string `json:"backend"` // 实际命中的提供方名称
	Model     string `json:"model,omitempty"`
	TokensIn  int    `json:"tokensIn"`
	TokensOut int    `json:"tokensOut"`
	Redacted  bool   `json:"redacted"` // 是否在出平台前做过脱敏
}

// AIProvider 统一能力接口（复用 ADR-005 适配器注册表范式）。
// 自托管 vLLM、第三方 API、未来本地小模型均实现此接口，由 Router 按策略选路。
type AIProvider interface {
	Name() string
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	Embed(ctx context.Context, tenantID string, texts []string) ([][]float32, error)
	Classify(ctx context.Context, tenantID string, text string, labels []string) (string, float64, error)
	Summarize(ctx context.Context, req ChatRequest) (string, error)
}
