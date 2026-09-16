//go:build webui

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"email-aggregator-go/src/notify"
	"email-aggregator-go/src/search"
	"email-aggregator-go/src/store"
)

// 内嵌 SPA 静态兜底的路由边界回归测试（仅 webui 标签）。
//
// 背景：mountStatic 以 mux.HandleFunc("/") 兜底**所有**未命中路径，一律回退 index.html
// 由前端路由接管 —— 这对 SPA 路由（刷新直达 /inbox）是必需的，但 `API` 与 `WS`
// 命名空间**必须排除**：这两个命名空间的路由都以更长前缀注册，能落到兜底处即代表
// 端点确实不存在。修复前它们同样返回 200 + text/html，调用方 JSON.parse 直接抛错，
// 一个"404 端点不存在"被伪装成"服务正常"。
//
// 该缺陷同时表现为两条既有测试在 -tags webui 下失败：
//   - observ.TestMetricsEndpoint_NotMounted（未挂载的 /api/metrics 期望 404，实得 200）
//   - TestDemoPushRequiresExplicitOptIn（未开启的 /api/demo/push 期望 404，实得 200）
func TestWebUIStaticFallbackRespectsAPINamespace(t *testing.T) {
	newServer := func() *ApiServer {
		return NewApiServer(
			store.NewInMemoryMetadataStore(),
			search.NewInMemorySearchIndex(),
			notify.NewInMemoryNotifier(),
			0,
		)
	}

	get := func(t *testing.T, srv *ApiServer, path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	isHTML := func(rec *httptest.ResponseRecorder) bool {
		return strings.Contains(rec.Header().Get("content-type"), "text/html")
	}

	// ---------- 1. 未注册的 API 路由：必须是 404，且**不能**是 SPA HTML ----------
	for _, path := range []string{
		"/api",
		"/api/this-route-does-not-exist",
		"/api/metrics", // 未挂载时
		"/api/demo/push",
	} {
		rec := get(t, newServer(), path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s：期望 404，实际 %d（content-type=%s）",
				path, rec.Code, rec.Header().Get("content-type"))
		}
		if isHTML(rec) {
			t.Errorf("GET %s：API 命名空间不得回退 SPA HTML（content-type=%s）",
				path, rec.Header().Get("content-type"))
		}
	}

	// ---------- 2. 未注册的 WS 路由：同样是 404 ----------
	recWS := get(t, newServer(), "/ws")
	if recWS.Code != http.StatusNotFound {
		t.Errorf("GET /ws（未挂载 hub）：期望 404，实际 %d", recWS.Code)
	}
	if isHTML(recWS) {
		t.Errorf("GET /ws：WS 命名空间不得回退 SPA HTML（content-type=%s）", recWS.Header().Get("content-type"))
	}

	// ---------- 3. 真实 API 路由仍然正常（确认修复没有把 API 误伤成 404）----------
	recHealth := get(t, newServer(), "/api/health")
	if recHealth.Code != http.StatusOK {
		t.Errorf("GET /api/health：期望 200，实际 %d", recHealth.Code)
	}
	if isHTML(recHealth) {
		t.Errorf("GET /api/health：不应返回 HTML（content-type=%s）", recHealth.Header().Get("content-type"))
	}

	// ---------- 4. SPA 前端路由仍必须回退 index.html，否则刷新即白屏 ----------
	for _, path := range []string{"/", "/inbox", "/settings/accounts"} {
		rec := get(t, newServer(), path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s：SPA 回退期望 200，实际 %d", path, rec.Code)
		}
		if !isHTML(rec) {
			t.Errorf("GET %s：SPA 回退应返回 text/html（content-type=%s）",
				path, rec.Header().Get("content-type"))
		}
		if !strings.Contains(rec.Body.String(), "<div id=\"root\">") {
			t.Errorf("GET %s：SPA 回退响应体不是 index.html（前 120 字节：%q）",
				path, truncate(rec.Body.String(), 120))
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
