package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestCanonicalMailJSONRoundTrip 验证 CanonicalMail 经过 marshal→unmarshal 后字段不丢失。
// 重点：tenantId（多租户数据面归属字段）必须随其它字段一起保真往返。
func TestCanonicalMailJSONRoundTrip(t *testing.T) {
	original := CanonicalMail{
		TenantID:      "tenant-abc",
		ID:            "mail-001",
		AccountID:     "acct-xyz",
		Provider:      ProviderIMAP,
		Folder:        "INBOX",
		From:          Address{Name: "Alice", Email: "alice@example.com"},
		To:            []Address{{Name: "Bob", Email: "bob@example.com"}},
		Cc:            []Address{{Name: "Carol", Email: "carol@example.com"}},
		Bcc:           []Address{{Name: "Dave", Email: "dave@example.com"}},
		Subject:       "测试主题",
		BodyText:      "hello world",
		BodyHTML:      "<p>hello world</p>",
		Snippet:       "hello…",
		HasAttachment: true,
		Attachments: []AttachmentMeta{
			{Filename: "a.txt", ContentType: "text/plain", SizeBytes: 12, ContentHash: "sha256:abc"},
		},
		InternalDate: 1700000000000,
		SizeBytes:    1024,
		Cursor: SyncCursor{
			UIDValidity:      1,
			UIDNext:          2,
			LastUID:          3,
			ModSeq:           99,
			HighWaterMark:    "hwm-1",
			ProviderSpecific: map[string]string{"k": "v"},
		},
		Read:         true,
		RawObjectKey: "obj/raw-001",
	}

	data, err := json.Marshal(&original)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var restored CanonicalMail
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if !canonicalEqual(original, restored) {
		t.Errorf("round-trip mismatch\noriginal: %+v\nrestored: %+v", original, restored)
	}
}

// TestCanonicalMailTenantIDPreserved 验证非空 tenantId 在序列化后保留，且能被还原。
func TestCanonicalMailTenantIDPreserved(t *testing.T) {
	m := CanonicalMail{TenantID: "tenant-123", ID: "m1", AccountID: "a1", Provider: ProviderGmail, Folder: "Inbox"}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"tenantId":"tenant-123"`) {
		t.Errorf("tenantId not serialized into JSON: %s", data)
	}

	var got CanonicalMail
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.TenantID != "tenant-123" {
		t.Errorf("tenantId lost in round-trip: got %q", got.TenantID)
	}
}

// TestCanonicalMailTenantIDOmittedWhenEmpty 验证空 tenantId 因 omitempty 被省略，
// 且往返后仍为""（不会凭空获得默认值，隔离边界由租户层兜底）。
func TestCanonicalMailTenantIDOmittedWhenEmpty(t *testing.T) {
	m := CanonicalMail{ID: "m1", AccountID: "a1", Provider: ProviderIMAP, Folder: "INBOX"}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "tenantId") {
		t.Errorf("empty tenantId should be omitted by omitempty, got: %s", data)
	}

	var got CanonicalMail
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.TenantID != "" {
		t.Errorf("expected empty tenantId after round-trip, got %q", got.TenantID)
	}
}

// TestCanonicalMailZeroValueAlwaysPresentFields 验证非空 omitempty 的必填/布尔/数值字段
// 即使零值也始终出现在 JSON 中（反序列化兜底依赖这些字段的显式存在）。
func TestCanonicalMailZeroValueAlwaysPresentFields(t *testing.T) {
	m := CanonicalMail{} // 全零值
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)

	// 这些字段没有 omitempty，零值也必须出现
	for _, want := range []string{
		`"id":""`,
		`"accountId":""`,
		`"provider":""`,
		`"folder":""`,
		`"subject":""`,
		`"hasAttachment":false`,
		`"internalDate":0`,
		`"sizeBytes":0`,
		`"read":false`,
		`"from":{"name":"","email":""}`,
		`"cursor":{}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("expected always-present field %q in JSON, got: %s", want, s)
		}
	}

	// 这些字段有 omitempty，零值应省略
	for _, absent := range []string{"tenantId", "bodyText", "bodyHTML", "snippet", "attachments", "rawObjectKey"} {
		if strings.Contains(s, absent) {
			t.Errorf("field %q should be omitted at zero value, got: %s", absent, s)
		}
	}
}

// TestAddressJSONRoundTrip 验证邮件地址结构往返保真。
func TestAddressJSONRoundTrip(t *testing.T) {
	a := Address{Name: "张三", Email: "z@example.com"}
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Address
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != a {
		t.Errorf("Address round-trip mismatch: got %+v want %+v", got, a)
	}
}

// TestSyncCursorJSONRoundTrip 验证断点续传游标（含可选 map）往返保真。
func TestSyncCursorJSONRoundTrip(t *testing.T) {
	c := SyncCursor{
		UIDValidity:      42,
		UIDNext:          43,
		LastUID:          41,
		ModSeq:           1000,
		HighWaterMark:    "mark-9",
		ProviderSpecific: map[string]string{"uv": "7", "tag": "x"},
	}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got SyncCursor
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Errorf("SyncCursor round-trip mismatch: got %+v want %+v", got, c)
	}
}

// TestAttachmentMetaJSONRoundTrip 验证附件元信息往返保真。
func TestAttachmentMetaJSONRoundTrip(t *testing.T) {
	a := AttachmentMeta{Filename: "f.pdf", ContentType: "application/pdf", SizeBytes: 2048, ContentHash: "sha256:deadbeef"}
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got AttachmentMeta
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != a {
		t.Errorf("AttachmentMeta round-trip mismatch: got %+v want %+v", got, a)
	}
}

// TestProviderConstants 验证服务商常量取值符合协议契约。
func TestProviderConstants(t *testing.T) {
	cases := map[Provider]string{
		ProviderIMAP:       "imap",
		ProviderPOP3:       "pop3",
		ProviderExchange:   "exchange",
		ProviderGmail:      "gmail",
		ProviderGraph:      "graph",
		ProviderEnterprise: "enterprise",
	}
	for p, want := range cases {
		if string(p) != want {
			t.Errorf("provider constant %q = %q, want %q", p, p, want)
		}
	}
}

// canonicalEqual 对比两个 CanonicalMail，使用 reflect.DeepEqual 语义，
// 但允许零值切片与 nil 切片被视为兼容（JSON 往返后 nil 切片保持 nil）。
func canonicalEqual(a, b CanonicalMail) bool {
	// 逐字段比较，对切片字段做 nil/empty 归一
	if a.TenantID != b.TenantID || a.ID != b.ID || a.AccountID != b.AccountID ||
		a.Provider != b.Provider || a.Folder != b.Folder || a.From != b.From ||
		!addrSliceEqual(a.To, b.To) || !addrSliceEqual(a.Cc, b.Cc) || !addrSliceEqual(a.Bcc, b.Bcc) ||
		a.Subject != b.Subject || a.BodyText != b.BodyText || a.BodyHTML != b.BodyHTML ||
		a.Snippet != b.Snippet || a.HasAttachment != b.HasAttachment ||
		!attachSliceEqual(a.Attachments, b.Attachments) ||
		a.InternalDate != b.InternalDate || a.SizeBytes != b.SizeBytes ||
		!reflect.DeepEqual(a.Cursor, b.Cursor) || a.Read != b.Read || a.RawObjectKey != b.RawObjectKey {
		return false
	}
	return true
}

func addrSliceEqual(a, b []Address) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func attachSliceEqual(a, b []AttachmentMeta) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
