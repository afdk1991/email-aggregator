package tenant

import (
	"net/http"
	"testing"
)

// TestFromRequestMissingHeader 关键隔离路径：缺少租户头时必须回退默认租户（"default"），
// 而非空串——空串会击穿多租户隔离边界。
func TestFromRequestMissingHeader(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	c := FromRequest(r)
	if c.TenantID != DefaultTenantID {
		t.Errorf("missing header should fall back to %q, got %q", DefaultTenantID, c.TenantID)
	}
	if c.Tier != "" {
		t.Errorf("missing tier header should be empty, got %q", c.Tier)
	}
}

// TestFromRequestWithHeader 显式租户头与层级头应被原样解析。
func TestFromRequestWithHeader(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set(HeaderTenantID, "tenant-xyz")
	r.Header.Set(HeaderTenantTier, "enterprise")
	c := FromRequest(r)
	if c.TenantID != "tenant-xyz" {
		t.Errorf("expected tenant-xyz, got %q", c.TenantID)
	}
	if c.Tier != "enterprise" {
		t.Errorf("expected enterprise tier, got %q", c.Tier)
	}
}

// TestFromRequestEmptyHeaderFallsBack 即使显式设置了空租户头，也应回退默认租户。
func TestFromRequestEmptyHeaderFallsBack(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set(HeaderTenantID, "") // 显式空值
	c := FromRequest(r)
	if c.TenantID != DefaultTenantID {
		t.Errorf("empty tenant header should fall back to %q, got %q", DefaultTenantID, c.TenantID)
	}
}

// TestResolve 规范化租户 ID：空值回退默认，非空原样返回。
func TestResolve(t *testing.T) {
	if got := Resolve(""); got != DefaultTenantID {
		t.Errorf("Resolve(\"\") = %q, want %q", got, DefaultTenantID)
	}
	if got := Resolve("tenant-a"); got != "tenant-a" {
		t.Errorf("Resolve(\"tenant-a\") = %q, want %q", got, "tenant-a")
	}
}

// TestContextString 验证 String() 输出格式，便于日志/审计观测。
func TestContextString(t *testing.T) {
	c := Context{TenantID: "t1", Tier: "public"}
	want := "tenant=t1 tier=public"
	if got := c.String(); got != want {
		t.Errorf("Context.String() = %q, want %q", got, want)
	}
	c2 := Context{TenantID: "t2", Tier: ""}
	want2 := "tenant=t2 tier="
	if got := c2.String(); got != want2 {
		t.Errorf("Context.String() = %q, want %q", got, want2)
	}
}

// TestConstants 验证默认租户与请求头键的契约取值。
func TestConstants(t *testing.T) {
	if DefaultTenantID != "default" {
		t.Errorf("DefaultTenantID = %q, want %q", DefaultTenantID, "default")
	}
	if HeaderTenantID != "X-Tenant-Id" {
		t.Errorf("HeaderTenantID = %q, want %q", HeaderTenantID, "X-Tenant-Id")
	}
	if HeaderTenantTier != "X-Tenant-Tier" {
		t.Errorf("HeaderTenantTier = %q, want %q", HeaderTenantTier, "X-Tenant-Tier")
	}
}
