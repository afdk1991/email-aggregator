// Package model 定义邮箱聚合平台的领域模型与统一契约（ADR-005）。
// 本包不依赖任何其它业务包，是 connector / sync / events / security 的公共基座。
package model

// Provider 邮件服务商协议类型
type Provider string

const (
	ProviderIMAP       Provider = "imap"
	ProviderPOP3       Provider = "pop3"
	ProviderExchange   Provider = "exchange"
	ProviderGmail      Provider = "gmail"
	ProviderEnterprise Provider = "enterprise"
)

// Address 邮件地址（RFC 5322）
type Address struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// AttachmentMeta 附件元信息（内容本身存对象存储，这里仅索引）
type AttachmentMeta struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	SizeBytes   int64  `json:"sizeBytes"`
	ContentHash string `json:"contentHash"`
}

// SyncCursor 断点续传游标（协议相关字段可选，多协议归一化）
type SyncCursor struct {
	UIDValidity      uint32            `json:"uidValidity,omitempty"`
	UIDNext          uint32            `json:"uidNext,omitempty"`
	LastUID          uint32            `json:"lastUid,omitempty"`
	ModSeq           uint64            `json:"modseq,omitempty"`
	HighWaterMark    string            `json:"highWaterMark,omitempty"`
	ProviderSpecific map[string]string `json:"providerSpecific,omitempty"`
}

// CanonicalMail 统一邮件模型（ADR-005 归一化产物，所有协议收敛于此）
type CanonicalMail struct {
	TenantID      string           `json:"tenantId,omitempty"` // ADR-009 多租户：数据面租户归属（存储/检索/通知全链路透传）
	ID            string           `json:"id"`
	AccountID     string           `json:"accountId"`
	Provider      Provider         `json:"provider"`
	Folder        string           `json:"folder"`
	From          Address          `json:"from"`
	To            []Address        `json:"to"`
	Cc            []Address        `json:"cc"`
	Bcc           []Address        `json:"bcc"`
	Subject       string           `json:"subject"`
	BodyText      string           `json:"bodyText,omitempty"`
	BodyHTML      string           `json:"bodyHTML,omitempty"`
	Snippet       string           `json:"snippet,omitempty"`
	HasAttachment bool             `json:"hasAttachment"`
	Attachments   []AttachmentMeta `json:"attachments,omitempty"`
	InternalDate  int64            `json:"internalDate"`
	SizeBytes     int64            `json:"sizeBytes"`
	Cursor        SyncCursor       `json:"cursor"`
	Read          bool             `json:"read"`                   // 用户已读状态（CRUD 生命周期）
	RawObjectKey  string           `json:"rawObjectKey,omitempty"` // 内容寻址对象存储引用
}
