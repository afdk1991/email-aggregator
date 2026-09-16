package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAccountValidOK 合法账户返回空错误串。
func TestAccountValidOK(t *testing.T) {
	a := &Account{ID: "acct-1", Provider: ProviderIMAP, Status: AccountActive}
	if msg := a.Valid(); msg != "" {
		t.Errorf("expected valid account, got error %q", msg)
	}
}

// TestAccountValidMissingID 缺少 ID 报错。
func TestAccountValidMissingID(t *testing.T) {
	a := &Account{Provider: ProviderIMAP, Status: AccountActive}
	if msg := a.Valid(); msg != "account id required" {
		t.Errorf("expected 'account id required', got %q", msg)
	}
}

// TestAccountValidMissingProvider 缺少 Provider 报错。
func TestAccountValidMissingProvider(t *testing.T) {
	a := &Account{ID: "acct-1", Status: AccountActive}
	if msg := a.Valid(); msg != "provider required" {
		t.Errorf("expected 'provider required', got %q", msg)
	}
}

// TestAccountValidInvalidStatus 非法状态报错。
func TestAccountValidInvalidStatus(t *testing.T) {
	a := &Account{ID: "acct-1", Provider: ProviderIMAP, Status: "weird"}
	if msg := a.Valid(); msg != "invalid status (active|paused|error)" {
		t.Errorf("expected invalid status error, got %q", msg)
	}
}

// TestAccountValidEmptyStatusDefaultsToActive 空状态被就地默认化为 active 且不报错。
// 注意：Valid() 会修改接收者 a.Status（副作用），这里显式断言该真实行为。
func TestAccountValidEmptyStatusDefaultsToActive(t *testing.T) {
	a := &Account{ID: "acct-1", Provider: ProviderIMAP} // Status 零值 ""
	if msg := a.Valid(); msg != "" {
		t.Errorf("expected valid with defaulted status, got %q", msg)
	}
	if a.Status != AccountActive {
		t.Errorf("empty status should default to %q, got %q", AccountActive, a.Status)
	}
}

// TestAccountValidStatusCaseSensitive 状态校验区分大小写，Active 不被接受。
func TestAccountValidStatusCaseSensitive(t *testing.T) {
	a := &Account{ID: "acct-1", Provider: ProviderIMAP, Status: "Active"}
	if msg := a.Valid(); msg == "" {
		t.Errorf("expected error for case-mismatched status, got none")
	}
}

// TestAccountJSONRoundTrip 验证账户模型往返保真，且空字段因 omitempty 省略。
func TestAccountJSONRoundTrip(t *testing.T) {
	a := Account{
		ID:             "acct-1",
		TenantID:       "tenant-9",
		Provider:       ProviderGraph,
		Email:          "u@example.com",
		DisplayName:    "User",
		Status:         AccountPaused,
		SyncFolder:     "INBOX",
		ServerHost:     "imap.example.com:993",
		CredentialsRef: "vault://ref-1",
		LastSyncAt:     1234567890,
		CreatedAt:      100,
		UpdatedAt:      200,
	}
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Account
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != a {
		t.Errorf("Account round-trip mismatch: got %+v want %+v", got, a)
	}
}

// TestAccountJSONOmitEmpty 验证零值可选字段被省略，但必填字段（id/provider/status/createdAt/updatedAt）始终存在。
func TestAccountJSONOmitEmpty(t *testing.T) {
	a := Account{ID: "acct-1", Provider: ProviderIMAP, Status: AccountActive, CreatedAt: 1, UpdatedAt: 2}
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	for _, want := range []string{`"id":"acct-1"`, `"provider":"imap"`, `"status":"active"`, `"createdAt":1`, `"updatedAt":2`} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in JSON, got: %s", want, s)
		}
	}
	for _, absent := range []string{"tenantId", "email", "displayName", "syncFolder", "serverHost", "credentialsRef", "lastSyncAt"} {
		if strings.Contains(s, absent) {
			t.Errorf("field %q should be omitted at zero value, got: %s", absent, s)
		}
	}
}
