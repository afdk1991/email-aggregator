package auth

import "testing"

func TestCan_PermissionMatrix(t *testing.T) {
	cases := []struct {
		role   Role
		action Action
		want   bool
	}{
		// owner 全权限
		{RoleOwner, ActionMailRead, true},
		{RoleOwner, ActionMailWrite, true},
		{RoleOwner, ActionAccountCreate, true},
		{RoleOwner, ActionAccountManage, true},
		{RoleOwner, ActionAdminTenant, true},
		// admin 缺租户管理
		{RoleAdmin, ActionMailRead, true},
		{RoleAdmin, ActionAccountManage, true},
		{RoleAdmin, ActionAdminTenant, false},
		// member 缺账户创建与租户管理
		{RoleMember, ActionMailRead, true},
		{RoleMember, ActionMailWrite, true},
		{RoleMember, ActionAccountManage, true},
		{RoleMember, ActionAccountCreate, false},
		{RoleMember, ActionAdminTenant, false},
		// viewer 仅读
		{RoleViewer, ActionMailRead, true},
		{RoleViewer, ActionMailWrite, false},
		{RoleViewer, ActionAccountManage, false},
		// 未知角色
		{Role("unknown"), ActionMailRead, false},
	}
	for _, c := range cases {
		got := Can(c.role, c.action)
		if got != c.want {
			t.Errorf("Can(%s,%s)=%v want %v", c.role, c.action, got, c.want)
		}
	}
}

func TestStaticResolver_GroupsPriority(t *testing.T) {
	r := NewStaticResolver()
	cases := []struct {
		groups []string
		want   Role
	}{
		{[]string{"owner"}, RoleOwner},
		{[]string{"admin"}, RoleAdmin},
		{[]string{"member"}, RoleMember},
		{[]string{"viewer"}, RoleViewer},
		{[]string{}, RoleViewer},
		{nil, RoleViewer},
		// 优先级 owner > admin > member
		{[]string{"admin", "owner"}, RoleOwner},
		{[]string{"member", "admin"}, RoleAdmin},
	}
	for _, c := range cases {
		got := r.Resolve("t1", "u1", c.groups)
		if got != c.want {
			t.Errorf("Resolve(groups=%v)=%v want %v", c.groups, got, c.want)
		}
	}
}
