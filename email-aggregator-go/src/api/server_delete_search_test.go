package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"email-aggregator-go/src/model"
	"email-aggregator-go/src/notify"
	"email-aggregator-go/src/search"
	"email-aggregator-go/src/store"
	"email-aggregator-go/src/tenant"
)

// TestDeleteMailAlsoRemovesFromSearch 回归测试：删除邮件必须同步清理检索索引，
// 避免删除后仍能搜到 stale 命中（存储/检索边界分离，删除在服务层编排二者）。
func TestDeleteMailAlsoRemovesFromSearch(t *testing.T) {
	metadata := store.NewInMemoryMetadataStore()
	idx := search.NewInMemorySearchIndex()
	notifier := notify.NewInMemoryNotifier()
	srv := NewApiServer(metadata, idx, notifier, 0)
	h := srv.Handler()

	const accountID = "acc_work"
	const mailID = "w-remove-1"
	mail := model.CanonicalMail{
		ID:           mailID,
		AccountID:    accountID,
		Provider:     model.ProviderIMAP,
		Folder:       "INBOX",
		From:         model.Address{Email: "boss@corp.com", Name: "老板"},
		Subject:      "待删除合同",
		BodyText:     "这是一份关于季度续约的合同正文，删除后不应再被检索到。",
		InternalDate: time.Now().Unix(),
		Cursor:       model.SyncCursor{LastUID: 1},
	}
	if err := metadata.UpsertMail(tenant.DefaultTenantID, mail); err != nil {
		t.Fatalf("seed UpsertMail: %v", err)
	}
	if err := idx.Index(tenant.DefaultTenantID, mail); err != nil {
		t.Fatalf("seed Index: %v", err)
	}

	// 删除前：检索应命中
	before := searchHits(t, h, accountID, "合同")
	if !containsID(before, mailID) {
		t.Fatalf("precondition failed: mail should be searchable before delete, got %v", before)
	}

	// 执行删除
	req := httptest.NewRequest(http.MethodDelete, "/api/mails/"+mailID+"?accountId="+accountID, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// 删除后：检索不应再命中（索引已同步清理）
	after := searchHits(t, h, accountID, "合同")
	if containsID(after, mailID) {
		t.Fatalf("stale hit: deleted mail %s still searchable, hits = %v", mailID, after)
	}

	// 删除后：详情也应 404（元数据已移除）
	det := httptest.NewRequest(http.MethodGet, "/api/mails/"+mailID+"?accountId="+accountID, nil)
	detRec := httptest.NewRecorder()
	h.ServeHTTP(detRec, det)
	if detRec.Code != http.StatusNotFound {
		t.Fatalf("GET deleted mail status = %d, want 404", detRec.Code)
	}
}

// TestDeleteMailCrossAccountGuard 回归测试：跨账户删除应 404，且不误删其他账户的索引文档。
func TestDeleteMailCrossAccountGuard(t *testing.T) {
	metadata := store.NewInMemoryMetadataStore()
	idx := search.NewInMemorySearchIndex()
	srv := NewApiServer(metadata, idx, notify.NewInMemoryNotifier(), 0)
	h := srv.Handler()

	mail := model.CanonicalMail{
		ID:        "x-1",
		AccountID: "acc_work",
		Subject:   "仅 work 可见",
		BodyText:  "跨账户删除守卫验证",
		Cursor:    model.SyncCursor{LastUID: 1},
	}
	_ = metadata.UpsertMail(tenant.DefaultTenantID, mail)
	_ = idx.Index(tenant.DefaultTenantID, mail)

	// 用错误账户删除：应 404，且 work 的索引文档仍可被 work 检索到
	req := httptest.NewRequest(http.MethodDelete, "/api/mails/x-1?accountId=acc_demo", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-account DELETE status = %d, want 404", rec.Code)
	}

	after := searchHits(t, h, "acc_work", "跨账户")
	if !containsID(after, "x-1") {
		t.Fatalf("cross-account delete wrongly removed index doc, hits = %v", after)
	}
}

func searchHits(t *testing.T, h http.Handler, accountID, q string) []map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/search?accountId="+accountID+"&q="+q, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("search status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Hits []map[string]any `json:"hits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	return body.Hits
}

func containsID(hits []map[string]any, id string) bool {
	for _, h := range hits {
		if h["idempotencyKey"] == id {
			return true
		}
	}
	return false
}

// TestTenantIsolationViaAPI 端到端验证 ADR-009 数据面租户隔离：
// 同一 account+id 的数据在不同 tenant 下互不穿透；缺省（无 X-Tenant-Id）走 default 租户。
func TestTenantIsolationViaAPI(t *testing.T) {
	metadata := store.NewInMemoryMetadataStore()
	idx := search.NewInMemorySearchIndex()
	srv := NewApiServer(metadata, idx, notify.NewInMemoryNotifier(), 0)
	h := srv.Handler()

	const accountID = "acc_shared"
	const mailID = "shared-1"
	mail := model.CanonicalMail{
		ID:        mailID,
		AccountID: accountID,
		Subject:   "租户隔离验证",
		BodyText:  "这封邮件只属于租户 t-a",
		Cursor:    model.SyncCursor{LastUID: 1},
	}
	// 直接以 t-a 租户落库 + 索引
	if err := metadata.UpsertMail("t-a", mail); err != nil {
		t.Fatalf("seed t-a: %v", err)
	}
	if err := idx.Index("t-a", mail); err != nil {
		t.Fatalf("seed index t-a: %v", err)
	}

	// 1) 以 t-a 租户查询：可见
	reqA := httptest.NewRequest(http.MethodGet, "/api/mails/"+mailID+"?accountId="+accountID, nil)
	reqA.Header.Set("X-Tenant-Id", "t-a")
	recA := httptest.NewRecorder()
	h.ServeHTTP(recA, reqA)
	if recA.Code != http.StatusOK {
		t.Fatalf("t-a GET mail status = %d, want 200", recA.Code)
	}

	// 2) 以 t-b 租户查询同 account+id：必须 404（隔离）
	reqB := httptest.NewRequest(http.MethodGet, "/api/mails/"+mailID+"?accountId="+accountID, nil)
	reqB.Header.Set("X-Tenant-Id", "t-b")
	recB := httptest.NewRecorder()
	h.ServeHTTP(recB, reqB)
	if recB.Code != http.StatusNotFound {
		t.Fatalf("t-b GET mail status = %d, want 404 (cross-tenant must be invisible)", recB.Code)
	}

	// 3) 以 t-b 租户检索：0 命中；以 t-a 检索：1 命中
	hitsB := searchHits(t, h, accountID, "租户")
	if containsID(hitsB, mailID) {
		t.Fatalf("t-b search must not see t-a mail, got %v", hitsB)
	}
	reqA2 := httptest.NewRequest(http.MethodGet, "/api/search?accountId="+accountID+"&q=租户", nil)
	reqA2.Header.Set("X-Tenant-Id", "t-a")
	recA2 := httptest.NewRecorder()
	h.ServeHTTP(recA2, reqA2)
	if recA2.Code != http.StatusOK {
		t.Fatalf("t-a search status = %d", recA2.Code)
	}
	var body struct {
		Hits []map[string]any `json:"hits"`
	}
	if err := json.Unmarshal(recA2.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !containsID(body.Hits, mailID) {
		t.Fatalf("t-a search must see its mail, got %v", body.Hits)
	}

	// 4) 无租户头（default）查询：同样不可见 t-a 数据，验证缺省不击穿隔离
	reqD := httptest.NewRequest(http.MethodGet, "/api/mails/"+mailID+"?accountId="+accountID, nil)
	recD := httptest.NewRecorder()
	h.ServeHTTP(recD, reqD)
	if recD.Code != http.StatusNotFound {
		t.Fatalf("default GET mail status = %d, want 404", recD.Code)
	}
}
