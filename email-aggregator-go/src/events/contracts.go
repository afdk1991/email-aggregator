// Package events 定义 Kafka 事件契约（5 类主题）与统一信封（ADR-002）。
// 事件体均可通过 JSON 序列化；消费端以 Envelope.Key 做幂等去重。
//
// ADR-009 多租户隔离：所有事件载荷强制携带 TenantID（omitempty 向后兼容旧事件，
// 消费端缺省回退 tenant.DefaultTenantID）。TenantID 与 AccountID 共同构成数据面
// 隔离边界——存储/检索/通知按 (tenantID, accountID) 双层命名空间路由。
package events

import "email-aggregator-go/src/model"

// 主题常量（与 TS PoC 完全一致）
const (
	TopicSyncTasks     = "sync-tasks"
	TopicMailIngested  = "mail-ingested"
	TopicMailIndex     = "mail-index"
	TopicNotifications = "notifications"
	TopicAudit         = "audit"
)

// SyncTaskEvent 触发某账号同步（initial / incremental）
// TenantID 透传：编排器据 SyncTaskEvent.TenantID 设置 busSink 与采集回传邮件的租户归属。
type SyncTaskEvent struct {
	TenantID  string           `json:"tenantId,omitempty"` // ADR-009
	AccountID string           `json:"accountId"`
	Mode      string           `json:"mode"`
	Cursor    model.SyncCursor `json:"cursor,omitempty"`
	Priority  int              `json:"priority,omitempty"`
}

// MailIngestedEvent 邮件已采集（触发落库 + 索引 + 通知）。
// 载荷携带足够索引/通知的字段（From/Subject/BodyText 可选），消费侧据此重建 CanonicalMail。
// TenantID 透传：IngestWorker.handle 据此把邮件落到正确租户的存储/索引/通知边界。
type MailIngestedEvent struct {
	TenantID     string `json:"tenantId,omitempty"` // ADR-009
	AccountID    string `json:"accountId"`
	MailID       string `json:"mailId"`
	Folder       string `json:"folder"`
	From         string `json:"from,omitempty"`
	Subject      string `json:"subject,omitempty"`
	BodyText     string `json:"bodyText,omitempty"`
	InternalDate int64  `json:"internalDate"`
	RawObjectKey string `json:"rawObjectKey,omitempty"`
	SizeBytes    int64  `json:"sizeBytes"`
	HasAttachment bool  `json:"hasAttachment"`
}

// MailIndexEvent 索引就绪
// TenantID 透传：供下游消费者（分析/审计）按租户归集。
type MailIndexEvent struct {
	TenantID  string `json:"tenantId,omitempty"` // ADR-009
	AccountID string `json:"accountId"`
	MailID    string `json:"mailId"`
	IndexedAt int64  `json:"indexedAt"`
}

// NotificationEvent 实时推送（WS/移动端）
// TenantID 透传：WS 转发器据此把通知路由到正确租户的连接（集成层 Notifier.Publish 需 tenantID 首参）。
type NotificationEvent struct {
	TenantID  string `json:"tenantId,omitempty"` // ADR-009
	AccountID string `json:"accountId"`
	Type      string `json:"type"` // new-mail | delete | update
	MailID    string `json:"mailId,omitempty"`
	Folder    string `json:"folder,omitempty"`
	At        int64  `json:"at"`
}

// AuditEvent 审计事件（合规）
// TenantID 透传：审计存储按租户分区，满足数据驻留合规约束。
type AuditEvent struct {
	TenantID string            `json:"tenantId,omitempty"` // ADR-009
	Actor    string            `json:"actor"`
	Action   string            `json:"action"`
	Target   string            `json:"target,omitempty"`
	At       int64             `json:"at"`
	Meta     map[string]string `json:"meta,omitempty"`
}

// EventEnvelope 统一信封：at-least-once 投递 + 幂等去重键
type EventEnvelope struct {
	Topic   string `json:"topic"`
	Key     string `json:"key"` // 幂等去重键（如 tenantId:accountId:mailId）
	TS      int64  `json:"ts"`
	Payload []byte `json:"payload"` // 序列化事件体（JSON）
}
