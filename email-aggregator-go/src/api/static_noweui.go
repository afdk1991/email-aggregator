//go:build !webui

package api

import "net/http"

// mountStatic 非 webui 构建（默认开发态）下为空操作：前端由 vite dev server
// 经代理提供，后端只暴露 API/WS，互不干扰。保持 Handler 调用处标签无关。
func (s *ApiServer) mountStatic(_ *http.ServeMux) {}
