package store

import (
	"testing"

	"email-aggregator-go/src/model"
	"email-aggregator-go/src/tenant"
)

// 元数据幂等写入 + 列表/计数（tenant_id 全链路首参）
func TestInMemoryMetadataStore_UpsertAndList(t *testing.T) {
	s := NewInMemoryMetadataStore()
	m := model.CanonicalMail{ID: "m1", AccountID: "acc1", Folder: "INBOX", Subject: "Hello", InternalDate: 100}
	if err := s.UpsertMail(tenant.DefaultTenantID, m); err != nil {
		t.Fatalf("upsert error: %v", err)
	}
	// 幂等：重复写入不应增加数量
	if err := s.UpsertMail(tenant.DefaultTenantID, m); err != nil {
		t.Fatalf("dup upsert error: %v", err)
	}
	got, err := s.ListMails(tenant.DefaultTenantID, "acc1", "INBOX", 10)
	if err != nil {
		t.Fatalf("list error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 mail after idempotent upsert, got %d", len(got))
	}
	n, _ := s.Count(tenant.DefaultTenantID, "acc1")
	if n != 1 {
		t.Fatalf("expected count 1, got %d", n)
	}
}

// 游标持久化往返
func TestInMemoryMetadataStore_Cursor(t *testing.T) {
	s := NewInMemoryMetadataStore()
	c := model.SyncCursor{LastUID: 42, UIDNext: 43}
	if err := s.PutCursor(tenant.DefaultTenantID, "acc1", "INBOX", c); err != nil {
		t.Fatalf("put cursor error: %v", err)
	}
	got, err := s.GetCursor(tenant.DefaultTenantID, "acc1", "INBOX")
	if err != nil {
		t.Fatalf("get cursor error: %v", err)
	}
	if got.LastUID != 42 {
		t.Fatalf("expected LastUID 42, got %d", got.LastUID)
	}
}

// 租户隔离：t-a 写入的数据在 t-b / default 租户下完全不可见
func TestInMemoryMetadataStore_TenantIsolation(t *testing.T) {
	s := NewInMemoryMetadataStore()
	base := model.CanonicalMail{ID: "m1", AccountID: "acc1", Folder: "INBOX", Subject: "x", InternalDate: 1}
	if err := s.UpsertMail("t-a", base); err != nil {
		t.Fatalf("upsert t-a: %v", err)
	}
	// t-a 有 1 封；t-b 与 default 应各为 0
	if n, _ := s.Count("t-a", "acc1"); n != 1 {
		t.Fatalf("t-a count = %d, want 1", n)
	}
	if n, _ := s.Count("t-b", "acc1"); n != 0 {
		t.Fatalf("t-b count = %d, want 0 (isolated)", n)
	}
	// 跨租户 GetMail 必须返回 nil
	if m, _ := s.GetMail("t-b", "acc1", "m1"); m != nil {
		t.Fatalf("cross-tenant GetMail must return nil, got %+v", m)
	}
	// 默认租户下同样不可见 t-a 数据
	if m, _ := s.GetMail(tenant.DefaultTenantID, "acc1", "m1"); m != nil {
		t.Fatalf("default tenant must not see t-a data, got %+v", m)
	}
	// t-a 自身可见
	if m, _ := s.GetMail("t-a", "acc1", "m1"); m == nil {
		t.Fatalf("t-a GetMail must return its own mail")
	}
}

// 内容存储：相同内容去重，键为 sha256
func TestInMemoryContentStore_Dedup(t *testing.T) {
	c := NewInMemoryContentStore()
	a, _ := c.Put([]byte("same body"))
	b, _ := c.Put([]byte("same body"))
	if a.ObjectKey != b.ObjectKey {
		t.Fatalf("expected dedup key, got %s vs %s", a.ObjectKey, b.ObjectKey)
	}
	back, err := c.Get(a.ObjectKey)
	if err != nil || string(back) != "same body" {
		t.Fatalf("get mismatch: %v %q", err, back)
	}
}
