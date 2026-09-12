// Package auth Phase 3 / 蓝图 §11 + ADR-009 网关层物理阻断：RBAC 静态矩阵 + OIDC SSO 集成。
//
// 设计定位：企业版鉴权与审计的最后一块拼图。本包提供
//   - Role/Action 静态矩阵（owner/admin/member/viewer × mail.read/mail.write/account.create/admin.tenant）
//   - RBACResolver 接口与 StaticResolver 实现（从 JWT groups claim 取角色）
//   - OIDCVerifier 接口（//go:build sso 标签下由 KeycloakVerifier/DexVerifier 实现）
//   - AuthMiddleware：Bearer 验签 → ctx 注入 Claims → Can 判定 → 403 时 emit AuditEvent
//
// ADR-009 硬约束：所有鉴权决策（allow/deny）必须 emit AuditEvent，事件 100% 带 tenantId，
// 跨租户访问在网关层物理阻断（篡改 JWT tenantId → 403 + action=cross-tenant-deny）。
//
// 零外部依赖：rbac.go / middleware.go / audit_sink.go 纯 stdlib；oidc.go 加 //go:build sso
// 引入 coreos/go-oidc + oauth2，保持 `go build ./...` 默认零依赖 PoC 能力。
package auth

// Role 租户内角色（企业版基线四角色，ADR-009）。
type Role string

const (
	RoleOwner  Role = "owner"  // 租户所有者：全权限 + 管理操作
	RoleAdmin  Role = "admin"  // 管理员：账户/凭据管理 + 邮件读写
	RoleMember Role = "member" // 成员：邮件读写 + 自有账户凭据录入
	RoleViewer Role = "viewer" // 只读：仅邮件查看
)

// Action 权限动作（粒度按业务关键操作划分）。
type Action string

const (
	ActionMailRead       Action = "mail.read"        // 邮件列表/详情/检索
	ActionMailWrite      Action = "mail.write"       // 邮件已读/删除/标记
	ActionAccountCreate  Action = "account.create"   // 新建账户
	ActionAccountManage  Action = "account.manage"   // 账户 CRUD + 凭据录入/同步触发
	ActionAdminTenant    Action = "admin.tenant"     // 租户管理（配额/成员/角色）
)

// permissionMatrix 静态权限矩阵：行=角色，列=动作。
// owner 全权限；admin 缺租户管理；member 缺账户创建与租户管理；viewer 仅读。
var permissionMatrix = map[Role]map[Action]bool{
	RoleOwner: {
		ActionMailRead: true, ActionMailWrite: true,
		ActionAccountCreate: true, ActionAccountManage: true, ActionAdminTenant: true,
	},
	RoleAdmin: {
		ActionMailRead: true, ActionMailWrite: true,
		ActionAccountCreate: true, ActionAccountManage: true,
	},
	RoleMember: {
		ActionMailRead: true, ActionMailWrite: true,
		ActionAccountManage: true, // 仅自有账户，由业务层二次校验
	},
	RoleViewer: {
		ActionMailRead: true,
	},
}

// Can 判定角色是否持有动作权限。
func Can(role Role, action Action) bool {
	acts, ok := permissionMatrix[role]
	if !ok {
		return false
	}
	return acts[action]
}

// RBACResolver 角色解析器：从用户身份（Claims）解析其在指定租户内的角色。
// StaticResolver 从 JWT groups claim 取角色；动态实现（casbin/OPA）留 Phase 4。
type RBACResolver interface {
	Resolve(tenantID, userID string, groups []string) Role
}

// StaticResolver 静态矩阵解析器：按 groups claim 映射角色。
//   - groups 含 "owner" → RoleOwner
//   - groups 含 "admin" → RoleAdmin
//   - groups 含 "member" → RoleMember
//   - 其余 → RoleViewer（最小权限原则）
type StaticResolver struct{}

// NewStaticResolver 构造静态角色解析器。
func NewStaticResolver() *StaticResolver { return &StaticResolver{} }

// Resolve 按 groups 优先级返回角色（owner > admin > member > viewer）。
func (s *StaticResolver) Resolve(_ string, _ string, groups []string) Role {
	has := func(r string) bool {
		for _, g := range groups {
			if g == r {
				return true
			}
		}
		return false
	}
	switch {
	case has(string(RoleOwner)):
		return RoleOwner
	case has(string(RoleAdmin)):
		return RoleAdmin
	case has(string(RoleMember)):
		return RoleMember
	default:
		return RoleViewer
	}
}
