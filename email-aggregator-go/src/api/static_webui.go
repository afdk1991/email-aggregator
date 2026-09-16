//go:build webui

package api

import (
	"io/fs"
	"net/http"
	"strings"

	"email-aggregator-go/webroot"
)

// mountStatic 将内嵌的前端 SPA 挂载到根路径，形成"单文件自包含"发布形态：
// 用户只需运行一个二进制即可同时获得 REST/WS API 与前端界面。
//
// API（/api/...）与 WS（/ws）路由已在 Handler 中先于 "/" 注册，Go 1.22 的
// ServeMux 按"最长匹配优先"分发，因此这些前缀不会被静态根拦截，互不冲突。
func (s *ApiServer) mountStatic(mux *http.ServeMux) {
	sub, err := fs.Sub(webroot.Dist, "dist")
	if err != nil {
		return
	}
	fileServer := http.FileServer(http.FS(sub))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")

		// API / WS 命名空间**不做** SPA 回退。这两个命名空间的路由都以更长的
		// 前缀注册，能落到这个兜底 handler 就说明该端点确实不存在。若照旧回退
		// index.html，调用方会拿到 200 + text/html：客户端 JSON.parse 直接抛错，
		// 而"404 端点不存在"被伪装成"服务正常"，排障时极具误导性。
		if isAPINamespace(p) {
			w.Header().Set("content-type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}

		if p == "" {
			serveIndex(w, sub)
			return
		}
		f, err := sub.Open(p)
		if err != nil {
			// 资源不存在 → 视为前端路由（如刷新直达 /inbox），回退 index.html 由 SPA 接管。
			serveIndex(w, sub)
			return
		}
		defer f.Close()
		if info, statErr := f.Stat(); statErr == nil && info.IsDir() {
			serveIndex(w, sub)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

// isAPINamespace 判断去掉首斜杠后的路径是否属于 API / WS 命名空间
// （"api"、"api/..."、"ws"、"ws/..."）。
func isAPINamespace(p string) bool {
	for _, ns := range []string{"api", "ws"} {
		if p == ns || strings.HasPrefix(p, ns+"/") {
			return true
		}
	}
	return false
}

func serveIndex(w http.ResponseWriter, sub fs.FS) {
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, "index.html not found (build with -tags webui)", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}
