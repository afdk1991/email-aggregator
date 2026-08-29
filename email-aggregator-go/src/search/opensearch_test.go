package search

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"email-aggregator-go/src/model"
	"email-aggregator-go/src/tenant"
)

// TestOpenSearchIndex_RealImpl 用 httptest 验证默认（//go:build 无标签）OpenSearchIndex
// 真实 REST 调用：索引名 tenant+account 隔离、Index/Search/Remove 请求路径正确、hits 解析正确。
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
		"POST /mail-default-acc_demo/_doc/m1",
		"DELETE /mail-default-acc_demo/_doc/m1",
		"POST /mail-default-acc_demo/_search",
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
