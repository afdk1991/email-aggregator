# =============================================================
# ci.ps1 —— 项目003 邮箱聚合平台 · 本地一键 CI
# -------------------------------------------------------------
# 运行三套实现的「构建 + 测试 + 类型/静态检查」并汇总结果：
#   1) TS PoC（email-aggregator）: demo + 单测 + tsc --noEmit
#   2) Go 主干（email-aggregator-go）: go build + go vet + go test
#   3) React 前端（email-aggregator-web）: npm run build
#
# 用法：
#   powershell -ExecutionPolicy Bypass -File .\scripts\ci.ps1
#   powershell -ExecutionPolicy Bypass -File .\scripts\ci.ps1 -Integration   # 追加 Go 集成构建
#
# 说明：TS 单测/运行需要 Node >=22.6（--experimental-strip-types）。
#   脚本优先使用托管 Node 22（.workbuddy），其次系统 Node；找不到则报错退出。
# =============================================================
param(
    [switch]$Integration   # 追加执行 Go 集成构建（go build -tags integration）
)

$Root  = Split-Path -Parent $PSScriptRoot
$LogDir = Join-Path $Root '.ci-logs'
New-Item -ItemType Directory -Force -Path $LogDir | Out-Null
$fails = 0

function Step($msg) { Write-Host "`n[$([DateTime]::Now.ToString('HH:mm:ss'))] $msg" -ForegroundColor Cyan }
function Ok($m)     { Write-Host "  [ok] $m" -ForegroundColor Green }
function Bad($m)    { Write-Host "  [FAIL] $m" -ForegroundColor Red; $script:fails++ }

# ── 定位 Node >=22.6 ──
$node = $null
$candidates = @(
    (Join-Path $env:USERPROFILE '.workbuddy\binaries\node\versions\22.22.2-2\node.exe'),
    (Join-Path $env:ProgramFiles 'nodejs\node.exe')
)
foreach ($c in $candidates) {
    if (Test-Path $c) {
        $v = & $c --version 2>$null
        if ($v -match 'v(\d+)\.') {
            $major = [int]$Matches[1]
            if ($major -ge 22) { $node = $c; break }
        }
    }
}
if (-not $node) { Write-Host "[FAIL] 未找到 Node >=22.6（--experimental-strip-types 必需）。请安装或配置托管 Node 22。" -ForegroundColor Red; exit 1 }
$npm = Join-Path (Split-Path $node) 'npm.cmd'
Write-Host "Node: $(& $node --version)  ($node)" -ForegroundColor Gray

# ── 1) TS PoC ──
Step '1/3  TS PoC（email-aggregator）'
Push-Location (Join-Path $Root 'email-aggregator')
try {
    & $node --experimental-strip-types src/demo.ts 2>&1 | Tee-Object -FilePath (Join-Path $LogDir 'ts-demo.log') | Select-Object -Last 3
    if ($LASTEXITCODE -eq 0) { Ok 'demo 端到端 PASS' } else { Bad "demo 失败 RC=$LASTEXITCODE" }

    & $node --test --experimental-strip-types tests/*.test.ts 2>&1 | Tee-Object -FilePath (Join-Path $LogDir 'ts-test.log') | Select-Object -Last 6
    if ($LASTEXITCODE -eq 0) { Ok '单测 PASS' } else { Bad "单测失败 RC=$LASTEXITCODE" }

    & $npm run typecheck 2>&1 | Tee-Object -FilePath (Join-Path $LogDir 'ts-typecheck.log') | Select-Object -Last 5
    if ($LASTEXITCODE -eq 0) { Ok 'tsc --noEmit PASS' } else { Bad "类型检查失败 RC=$LASTEXITCODE" }
} finally { Pop-Location }

# ── 2) Go 主干 ──
Step '2/3  Go 主干（email-aggregator-go）'
Push-Location (Join-Path $Root 'email-aggregator-go')
try {
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) { Bad '未找到 go 命令' }
    else {
        go build ./... 2>&1 | Tee-Object -FilePath (Join-Path $LogDir 'go-build.log') | Select-Object -Last 5
        if ($LASTEXITCODE -eq 0) { Ok 'go build ./... PASS' } else { Bad "go build 失败 RC=$LASTEXITCODE" }

        go vet ./... 2>&1 | Tee-Object -FilePath (Join-Path $LogDir 'go-vet.log') | Select-Object -Last 5
        if ($LASTEXITCODE -eq 0) { Ok 'go vet ./... PASS' } else { Bad "go vet 失败 RC=$LASTEXITCODE" }

        go test ./... 2>&1 | Tee-Object -FilePath (Join-Path $LogDir 'go-test.log') | Select-Object -Last 6
        if ($LASTEXITCODE -eq 0) { Ok 'go test ./... PASS' } else { Bad "go test 失败 RC=$LASTEXITCODE" }

        if ($Integration) {
            $env:GOFLAGS = '-mod=mod'
            $env:GOPATH = Join-Path $Root 'email-aggregator-go\.gopath'
            $env:GOMODCACHE = Join-Path $Root 'email-aggregator-go\.gopath\pkg\mod'
            $env:GOPROXY = 'https://goproxy.cn,direct'
            go build -tags integration ./... 2>&1 | Tee-Object -FilePath (Join-Path $LogDir 'go-integ-build.log') | Select-Object -Last 5
            if ($LASTEXITCODE -eq 0) { Ok 'go build -tags integration PASS' } else { Bad "集成构建失败 RC=$LASTEXITCODE" }
        }
    }
} finally { Pop-Location }

# ── 3) React 前端 ──
Step '3/3  React 前端（email-aggregator-web）'
Push-Location (Join-Path $Root 'email-aggregator-web')
try {
    if (-not (Test-Path node_modules)) { & $npm ci 2>&1 | Out-Null }
    & $npm run build 2>&1 | Tee-Object -FilePath (Join-Path $LogDir 'web-build.log') | Select-Object -Last 8
    if ($LASTEXITCODE -eq 0) { Ok 'npm run build PASS' } else { Bad "前端构建失败 RC=$LASTEXITCODE" }
} finally { Pop-Location }

# ── 汇总 ──
Write-Host "`n=============================================" -ForegroundColor Cyan
if ($fails -eq 0) { Write-Host "CI 全绿：所有构建/测试/类型检查通过" -ForegroundColor Green }
else { Write-Host "CI 存在 $fails 项失败（日志见 .ci-logs/）" -ForegroundColor Red }
Write-Host "=============================================" -ForegroundColor Cyan
exit ($(if ($fails -gt 0) { 1 } else { 0 }))
