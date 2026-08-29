//go:build webui

// Package webroot 内嵌已构建的前端 SPA（email-aggregator-web/dist）。
//
// 仅在使用 `-tags webui` 构建时参与编译，因此默认 `go build ./...`
// 无需前端产物即可通过（webroot 包在该标签下零文件，不进入编译）。
// 发布流水线（build.sh / build.ps1）会把 web/dist 复制到本目录的 dist/ 下再编译。
package webroot

import "embed"

// Dist 内嵌的前端静态资源根（构建时由流水线注入 dist/）。
//
//go:embed all:dist
var Dist embed.FS
