package model

import "context"

// ConnectorCapabilities 适配器能力声明（供编排器做降级/路由决策）
type ConnectorCapabilities struct {
	Provider            Provider        `json:"provider"`
	SupportsIncremental bool            `json:"supportsIncremental"`
	SupportsIdle        bool            `json:"supportsIdle"`
	SupportsQresync     bool            `json:"supportsQresync"`
	SupportsCondstore   bool            `json:"supportsCondstore"`
	SupportsOAuth       bool            `json:"supportsOAuth"`
	SupportsUIDL        bool            `json:"supportsUIDL"`
	Features            map[string]bool `json:"features,omitempty"`
}

// SyncSink 同步结果回传接口（由采集层实现，适配器只负责"生产"邮件）
type SyncSink interface {
	OnMessage(ctx context.Context, m CanonicalMail) error
	OnDelete(ctx context.Context, id string) error
}

// OAuthToken OAuth2 令牌（PKCE 流程产物）
type OAuthToken struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
}

// Credential 连接凭据（仅运行时内存态；落库必须走 Vault 信封加密，ADR-004）
type Credential struct {
	Type     string            `json:"type"` // oauth2 | password | app-password
	Username string            `json:"username,omitempty"`
	Password string            `json:"password,omitempty"`
	OAuth    *OAuthToken       `json:"oauth,omitempty"`
	Extra    map[string]string `json:"extra,omitempty"`
}

// Connector 协议接入适配器接口（ADR-005 核心契约，所有协议实现此接口）
type Connector interface {
	Connect(ctx context.Context, cred Credential) error
	Capabilities() ConnectorCapabilities
	// InitialFullSync 初始全量：拉取自 since 以来的全部邮件，分页经 sink 回传
	InitialFullSync(ctx context.Context, since int64, sink SyncSink) error
	// IncrementalSync 增量：从 cursor 断点继续
	IncrementalSync(ctx context.Context, cursor SyncCursor, sink SyncSink) error
	// StreamChanges 长连接推送（如 IMAP IDLE）：有变更时经 onEvent 通知
	StreamChanges(ctx context.Context, cursor SyncCursor, onEvent func(ctx context.Context, m CanonicalMail) error) error
	Close() error
}

// ConnectorFactory 构造适配器（注册表使用）
type ConnectorFactory func(provider Provider, cfg map[string]string) (Connector, error)
