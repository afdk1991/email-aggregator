package store

import (
	"testing"

	"email-aggregator-go/src/model"
)

func TestInMemoryAccountStore_CRUD(t *testing.T) {
	s := NewInMemoryAccountStore()

	// Create
	a := model.Account{ID: "acc_1", Provider: model.ProviderIMAP, Email: "a@example.com", Status: model.AccountActive, SyncFolder: "INBOX"}
	if err := s.CreateAccount("default", a); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Duplicate create rejected
	if err := s.CreateAccount("default", a); err == nil {
		t.Fatal("duplicate create should error")
	}

	// Get
	got, err := s.GetAccount("default", "acc_1")
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.Email != "a@example.com" || got.Provider != model.ProviderIMAP {
		t.Fatalf("unexpected account: %+v", got)
	}

	// Update（整记录替换；部分更新语义由 API 层合并 existing 后再落库）
	if err := s.UpdateAccount("default", model.Account{ID: "acc_1", Provider: model.ProviderIMAP, Email: "a@example.com", Status: model.AccountPaused, SyncFolder: "INBOX"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = s.GetAccount("default", "acc_1")
	if got.Status != model.AccountPaused || got.Email != "a@example.com" {
		t.Fatalf("update lost fields: %+v", got)
	}

	// List
	list, err := s.ListAccounts("default")
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v", err)
	}

	// Tenant isolation
	if err := s.CreateAccount("tenantB", model.Account{ID: "acc_1", Provider: model.ProviderIMAP, Status: model.AccountActive}); err != nil {
		t.Fatalf("create tenantB: %v", err)
	}
	listB, _ := s.ListAccounts("tenantB")
	if len(listB) != 1 {
		t.Fatalf("tenant isolation broken: %+v", listB)
	}

	// Delete
	if err := s.DeleteAccount("default", "acc_1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, _ := s.GetAccount("default", "acc_1"); got != nil {
		t.Fatal("expected nil after delete")
	}
	if err := s.DeleteAccount("default", "acc_1"); err == nil {
		t.Fatal("delete missing should error")
	}
}

func TestInMemoryAccountStore_Upsert(t *testing.T) {
	s := NewInMemoryAccountStore()
	a := model.Account{ID: "acc_x", Provider: model.ProviderPOP3, Status: model.AccountActive}
	if err := s.Upsert("default", a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Second upsert idempotent (no error)
	if err := s.Upsert("default", a); err != nil {
		t.Fatalf("upsert repeat: %v", err)
	}
	got, _ := s.GetAccount("default", "acc_x")
	if got == nil || got.Provider != model.ProviderPOP3 {
		t.Fatalf("unexpected: %+v", got)
	}
	if got.CreatedAt == 0 || got.UpdatedAt == 0 {
		t.Fatalf("timestamps not set: %+v", got)
	}
}
