package model

// AccountStatus 账户生命周期状态（连接/账户服务，ADR-005/ADR-009）。
type AccountStatus string

const (
	AccountActive AccountStatus = "active" // 正常同步
	AccountPaused AccountStatus = "paused" // 用户暂停（同步编排器跳过）
	AccountError  AccountStatus = "error"  // 连接失败/鉴权失效，需人工介入
)

// Account 账户注册表条目：账户身份 + 连接配置引用 + 生命周期状态。
// 凭据不在此存储明文——仅保存 credentialsRef（指向 Token Vault/KMS 信封加密引用，ADR-004）。
type Account struct {
	ID             string        `json:"id"`
	TenantID       string        `json:"tenantId,omitempty"`
	Provider       Provider      `json:"provider"`
	Email          string        `json:"email,omitempty"`
	DisplayName    string        `json:"displayName,omitempty"`
	Status         AccountStatus `json:"status"`
	SyncFolder     string        `json:"syncFolder,omitempty"`   // 默认同步文件夹（如 INBOX / Inbox）
	CredentialsRef string        `json:"credentialsRef,omitempty"` // Token Vault/KMS 引用，非明文
	LastSyncAt     int64         `json:"lastSyncAt,omitempty"`
	CreatedAt      int64         `json:"createdAt"`
	UpdatedAt      int64         `json:"updatedAt"`
}

// Valid 校验必填字段，返回可读错误（空则合法）。
func (a *Account) Valid() string {
	if a.ID == "" {
		return "account id required"
	}
	if a.Provider == "" {
		return "provider required"
	}
	if a.Status == "" {
		a.Status = AccountActive
	}
	switch a.Status {
	case AccountActive, AccountPaused, AccountError:
	default:
		return "invalid status (active|paused|error)"
	}
	return ""
}
