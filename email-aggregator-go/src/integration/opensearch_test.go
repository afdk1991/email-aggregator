//go:build integration

package integration

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"email-aggregator-go/src/model"
)

// TestOpenSearchIndex_IndexSearchRemove 用 httptest 模拟 OpenSearch，
// 验证 integration.OpenSearchIndex 的 Index/Search/Remove 真正发出正确请求并正确解析响应。
func TestOpenSearchIndex_IndexSearchRemove(t *testing.T) {
	var indexReq, searchReq, removeReq capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/_doc/"):
			indexReq = capturedRequest{Method: r.Method, Path: r.URL.Path, Body: string(body)}
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"_id":"m1","result":"created"}`))
		case strings.HasSuffix(r.URL.Path, "/_search"):
			searchReq = capturedRequest{Method: r.Method, Path: r.URL.Path, Body: string(body)}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(searchResponseJSON()))
		case r.Method == http.MethodDelete:
			removeReq = capturedRequest{Method: r.Method, Path: r.URL.Path, Body: string(body)}
			w.WriteHeader(404) // 文档不存在，按幂等删除视为成功
		default:
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()

	idx, err := NewOpenSearchIndex(srv.URL, "", "")
	if err != nil {
		t.Fatalf("NewOpenSearchIndex: %v", err)
	}

	// ── Index ──
	m := model.CanonicalMail{
		ID:           "m1",
		AccountID:    "acc1",
		Subject:      "hello",
		From:         model.Address{Email: "a@b.com"},
		BodyText:     "world body",
		InternalDate: 123456,
	}
	if err := idx.Index("default", m); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if indexReq.Method != http.MethodPost {
		t.Errorf("index method = %q, want POST", indexReq.Method)
	}
	if !strings.Contains(indexReq.Path, "mail-default/_doc/m1") {
		t.Errorf("index path = %q, want contains mail-default/_doc/m1", indexReq.Path)
	}
	// ADR-009 起索引按**租户**物理隔离（mail-<tenantId>），accountId 不再出现在索引名里
	// （避免"每账户一索引"造成索引爆炸）。账户归属下沉为文档字段，故此处必须断言
	// 它确实被写进了文档体 —— 否则该文档将无法被任何账户检索到。
	if strings.Contains(indexReq.Path, "acc1") {
		t.Errorf("index path = %q, want NO accountId in index name (ADR-009)", indexReq.Path)
	}
	if !strings.Contains(indexReq.Body, `"accountId":"acc1"`) {
		t.Errorf("index body missing accountId field: %s", indexReq.Body)
	}

	// ── Search ──
	hits, err := idx.Search("default", "acc1", "hello", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if searchReq.Method != http.MethodPost {
		t.Errorf("search method = %q, want POST", searchReq.Method)
	}
	if !strings.Contains(searchReq.Path, "mail-default/_search") {
		t.Errorf("search path = %q, want contains mail-default/_search", searchReq.Path)
	}
	// ADR-009 起"同租户多账户"的隔离完全落在 bool.filter.term.accountId 上：
	// 索引名已不含 accountId，一旦这个 term 过滤丢失，同租户下不同账户的邮件会互相串号。
	if !strings.Contains(searchReq.Body, `"accountId":"acc1"`) {
		t.Errorf("search body missing accountId term filter (cross-account leak risk): %s", searchReq.Body)
	}
	if !strings.Contains(searchReq.Body, "multi_match") {
		t.Errorf("search body missing multi_match: %s", searchReq.Body)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	if hits[0].Subject != "hello" {
		t.Errorf("hit subject = %q, want hello", hits[0].Subject)
	}
	if hits[0].AccountID != "acc1" {
		t.Errorf("hit accountId = %q, want acc1", hits[0].AccountID)
	}
	if hits[0].From != "a@b.com" {
		t.Errorf("hit from = %q, want a@b.com", hits[0].From)
	}
	if hits[0].InternalDate != 123456 {
		t.Errorf("hit internalDate = %d, want 123456", hits[0].InternalDate)
	}

	// ── Remove（404 视为成功）──
	if err := idx.Remove("default", "acc1", "m1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if removeReq.Method != http.MethodDelete {
		t.Errorf("remove method = %q, want DELETE", removeReq.Method)
	}
	if !strings.Contains(removeReq.Path, "mail-default/_doc/m1") {
		t.Errorf("remove path = %q, want contains mail-default/_doc/m1", removeReq.Path)
	}
}

type capturedRequest struct {
	Method string
	Path   string
	Body   string
}

func searchResponseJSON() string {
	return `{"hits":{"hits":[{"_id":"m1","_source":{"tenantId":"default","accountId":"acc1","subject":"hello","from":"a@b.com","bodyText":"world body","internalDate":123456}}]}}`
}
