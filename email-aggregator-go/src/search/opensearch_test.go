package search

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"email-aggregator-go/src/model"
	"email-aggregator-go/src/tenant"
)

// TestOpenSearchIndex_RealImpl 用 httptest 验证默认（//go:build 无标签）OpenSearchIndex
// 真实 REST 调用：Phase 2 / ADR-009 per-tenant 物理索引 mail-<tid>、Index/Search/Remove
// 请求路径正确、Search 查询体含 accountId filter、hits 解析正确。
func TestOpenSearchIndex_RealImpl(t *testing.T) {
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/_doc/m1"):
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/_search"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"hits": map[string]any{"hits": []any{
					map[string]any{
						"_id": "m1",
						"_source": map[string]any{
							"accountId":    "acc_demo",
							"subject":      "Welcome onboard",
							"from":         "alice@example.com",
							"bodyText":     "welcome to the platform",
							"internalDate": 1700000000,
						},
					},
				}},
			})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	idx := NewOpenSearchIndex(srv.URL, "", "")

	m := model.CanonicalMail{
		ID: "m1", AccountID: "acc_demo", Folder: "INBOX", Provider: model.ProviderIMAP,
		From: model.Address{Email: "alice@example.com"}, Subject: "Welcome onboard",
		BodyText: "welcome to the platform", InternalDate: 1700000000,
	}
	if err := idx.Index(tenant.DefaultTenantID, m); err != nil {
		t.Fatalf("Index err: %v", err)
	}
	if err := idx.Remove(tenant.DefaultTenantID, "acc_demo", "m1"); err != nil {
		t.Fatalf("Remove err: %v", err)
	}
	hits, err := idx.Search(tenant.DefaultTenantID, "acc_demo", "welcome", 10)
	if err != nil {
		t.Fatalf("Search err: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "m1" || hits[0].Subject != "Welcome onboard" {
		t.Fatalf("unexpected hits: %+v", hits)
	}

	want := []string{
		"POST /mail-default/_doc/m1",
		"DELETE /mail-default/_doc/m1",
		"POST /mail-default/_search",
	}
	if len(gotPaths) != len(want) {
		t.Fatalf("got paths %v want %v", gotPaths, want)
	}
	for i := range want {
		if gotPaths[i] != want[i] {
			t.Fatalf("path[%d]=%q want %q", i, gotPaths[i], want[i])
		}
	}
}

// TestOpenSearchIndex_PerTenantIndex 验证 Phase 2 / ADR-009 per-tenant 物理索引：
// 不同 tenantID 路由到不同索引（mail-tenantA vs mail-tenantB），同一 tenant 多账户共用索引；
// Search 查询体含 bool.filter.term.accountId 过滤项。
func TestOpenSearchIndex_PerTenantIndex(t *testing.T) {
	var capturedIndex, capturedSearchBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedIndex = r.URL.Path
		if strings.HasSuffix(r.URL.Path, "/_search") {
			b, _ := io.ReadAll(r.Body)
			capturedSearchBody = string(b)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hits": map[string]any{"hits": []any{}},
		})
	}))
	defer srv.Close()

	idx := NewOpenSearchIndex(srv.URL, "", "")

	// 同一租户、两个账户 → 应该共用 mail-tenanta
	mA := model.CanonicalMail{ID: "m1", AccountID: "acc_a", Subject: "subjA", InternalDate: 1}
	mB := model.CanonicalMail{ID: "m2", AccountID: "acc_b", Subject: "subjB", InternalDate: 2}
	if err := idx.Index("tenantA", mA); err != nil {
		t.Fatalf("Index A err: %v", err)
	}
	if !strings.Contains(capturedIndex, "/mail-tenanta/_doc/m1") {
		t.Fatalf("expected mail-tenanta index, got %s", capturedIndex)
	}
	if err := idx.Index("tenantA", mB); err != nil {
		t.Fatalf("Index B err: %v", err)
	}
	if !strings.Contains(capturedIndex, "/mail-tenanta/_doc/m2") {
		t.Fatalf("expected mail-tenanta index for second account, got %s", capturedIndex)
	}

	// 不同租户 → 应该路由到 mail-tenantb
	mC := model.CanonicalMail{ID: "m3", AccountID: "acc_c", Subject: "subjC", InternalDate: 3}
	if err := idx.Index("tenantB", mC); err != nil {
		t.Fatalf("Index C err: %v", err)
	}
	if !strings.Contains(capturedIndex, "/mail-tenantb/_doc/m3") {
		t.Fatalf("expected mail-tenantb index for different tenant, got %s", capturedIndex)
	}

	// Search 查询体必须含 accountId filter
	if _, err := idx.Search("tenantA", "acc_a", "subj", 10); err != nil {
		t.Fatalf("Search err: %v", err)
	}
	if !strings.Contains(capturedSearchBody, `"accountId"`) {
		t.Fatalf("Search body missing accountId filter: %s", capturedSearchBody)
	}
	if !strings.Contains(capturedSearchBody, `"multi_match"`) {
		t.Fatalf("Search body missing multi_match: %s", capturedSearchBody)
	}
}
