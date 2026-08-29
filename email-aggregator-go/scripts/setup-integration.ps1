# setup-integration.ps1
# 拉取 Go 集成构建所需的外部依赖并生成 go.sum。
# 前置：Go 1.22+ 已安装且在 PATH（本项目已验证 go1.22.5）。
# 说明：依赖已钉版到兼容 go 1.22 的版本；本机直连 proxy.golang.org 不可达，
#       故统一走 goproxy.cn 镜像（如更换环境可改回官方代理）。
# 用法：在 email-aggregator-go/ 目录下执行
#   pwsh scripts/setup-integration.ps1
# 之后即可：go build -tags integration ./...

$ErrorActionPreference = "Stop"

# 将 deploy/.env 注入当前进程环境变量（供 os.Getenv 读取）
$envFile = Join-Path $PSScriptRoot ".." "deploy" ".env"
if (Test-Path $envFile) {
    Write-Host "Loading env from $envFile"
    Get-Content $envFile | Where-Object { $_ -match '^\s*[^#].*=' } | ForEach-Object {
        $parts = $_ -split '=', 2
        if ($parts.Count -eq 2) {
            [Environment]::SetEnvironmentVariable($parts[0].Trim(), $parts[1].Trim())
        }
    }
} else {
    Write-Warning "deploy/.env not found; integration build will use built-in defaults"
}

# 统一构建环境：走国内可达镜像、禁用 sumdb 校验与工具链自动升级
$env:GOPROXY = "https://goproxy.cn"
$env:GOSUMDB = "off"
$env:GOTOOLCHAIN = "local"

# 钉版到兼容 go 1.22 的依赖（避免 @latest 把 go.mod 顶到 1.25）。
# 若 go.mod 已被破坏，可直接用 go mod edit 写回这些版本后 go mod tidy。
$deps = @(
    "github.com/segmentio/kafka-go@v0.4.47",
    "github.com/jackc/pgx/v5@v5.7.4",
    "github.com/opensearch-project/opensearch-go/v2@v2.3.0",
    "github.com/minio/minio-go/v7@v7.0.66",
    "github.com/gorilla/websocket@v1.5.3",
    "golang.org/x/crypto@v0.21.0",
    "golang.org/x/net@v0.23.0",
    "golang.org/x/sys@v0.18.0",
    "golang.org/x/text@v0.14.0",
    "golang.org/x/sync@v0.6.0",
    "github.com/klauspost/compress@v1.17.11",
    "github.com/klauspost/crc32@v1.2.0",
    "github.com/tinylib/msgp@v1.1.9"
)

Push-Location (Join-Path $PSScriptRoot "..")
try {
    foreach ($d in $deps) {
        Write-Host "go get $d"
        go get $d
    }
    Write-Host "go mod tidy"
    go mod tidy
    Write-Host "Done. Integration deps are ready."
} finally {
    Pop-Location
}
