# 一键重新部署「邮箱聚合平台」到 EdgeOne Makers（Windows / PowerShell 版）
#
# 用法：
#   powershell -ExecutionPolicy Bypass -File .\deploy.ps1
#   powershell -ExecutionPolicy Bypass -File .\deploy.ps1 -ProjectName my-project
#   $env:EDGEONE_TOKEN="xxx"; powershell -ExecutionPolicy Bypass -File .\deploy.ps1
param(
    [string]$ProjectName = "email-aggregator-p003"
)

$ErrorActionPreference = "Stop"
$env:PAGES_SOURCE = "skills"

$Here = Split-Path -Parent $MyInvocation.MyCommand.Path
$Root = Resolve-Path (Join-Path $Here "..\..")
$Web = Join-Path $Root "email-aggregator-web"

Write-Host "==> [1/4] 构建前端" -ForegroundColor Cyan
Push-Location $Web
try { npm run build } finally { Pop-Location }
if ($LASTEXITCODE -ne 0) { throw "前端构建失败" }

Write-Host "==> [2/4] 同步构建产物到部署根" -ForegroundColor Cyan
Copy-Item (Join-Path $Web "dist\index.html") (Join-Path $Here "index.html") -Force
$assets = Join-Path $Here "assets"
if (Test-Path $assets) { Remove-Item $assets -Recurse -Force }
New-Item -ItemType Directory -Path $assets | Out-Null
Copy-Item (Join-Path $Web "dist\assets\*") $assets -Force
Write-Host "    静态资源已就位: $((Get-ChildItem $assets).Count) 个文件"

Write-Host "==> [2.5/4] 恢复项目绑定（防止 CLI 按名字另建项目）" -ForegroundColor Cyan
$edgeoneDir = Join-Path $Here ".edgeone"
if (-not (Test-Path $edgeoneDir)) { New-Item -ItemType Directory -Path $edgeoneDir | Out-Null }
Copy-Item (Join-Path $Here "edgeone-project.json") (Join-Path $edgeoneDir "project.json") -Force
$pid2 = (Get-Content (Join-Path $Here "edgeone-project.json") | ConvertFrom-Json).ProjectId
Write-Host "    绑定 ProjectId: $pid2"

Write-Host "==> [3/4] 本地冒烟测试（演示形态 + 生产形态各一套）" -ForegroundColor Cyan
Push-Location $Here
try {
    node smoke.mjs
    if ($LASTEXITCODE -ne 0) { throw "演示形态冒烟未通过，已中止部署" }
    node smoke.mjs --prod
    if ($LASTEXITCODE -ne 0) { throw "生产形态冒烟未通过，已中止部署" }
} finally { Pop-Location }

Write-Host "==> [4/4] 部署到 EdgeOne Makers" -ForegroundColor Cyan
Push-Location $Here
try {
    if ($env:EDGEONE_TOKEN) {
        edgeone makers deploy -n $ProjectName -t $env:EDGEONE_TOKEN --json
    } else {
        edgeone makers deploy -n $ProjectName --json
    }
} finally { Pop-Location }
