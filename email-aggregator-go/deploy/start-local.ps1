# =============================================================
# start-local.ps1 —— 邮箱聚合平台 一键本地启动
# -------------------------------------------------------------
# 作用：本机 Docker Desktop 首次启动不稳定，因此本脚本改用
#   Ubuntu WSL 内原生 dockerd 作为 Docker 引擎，并在此之上
#   拉起 PG / MinIO / OpenSearch / Kafka 四件套 + 一键初始化，
#   最后以「自包含」形态运行集成服务（同端口提供 SPA + REST + WS）。
# 用法：
#   powershell -ExecutionPolicy Bypass -File .\deploy\start-local.ps1
#   可选：-Port 8082（服务端口） / -Rebuild（强制重编 Go 服务）
# 停止：powershell -ExecutionPolicy Bypass -File .\deploy\stop-local.ps1
# =============================================================
param(
    [int]$Port = 8082,
    [switch]$Rebuild
)
# 注意：不能用 $ErrorActionPreference='Stop' —— dockerd 每次经 TCP 连接都会向 stderr
# 输出「unencrypted API」弃用提示，EAP=Stop 会把它误判为终止错误。改用显式 $LASTEXITCODE 检查。
$ErrorActionPreference = 'Continue'

$Deploy   = $PSScriptRoot
$Root     = Split-Path -Parent $PSScriptRoot      # email-aggregator-go/
$BinDir   = Join-Path $Root 'bin'
$ServerExe = Join-Path $BinDir 'integ-webui.exe'
$WslDist  = 'Ubuntu'
$WslDeploy = '/mnt/d/网站全栈项目/项目003/email-aggregator-go/deploy'

function Step($msg) { Write-Host "[$([DateTime]::Now.ToString('HH:mm:ss'))] $msg" -ForegroundColor Cyan }
function Ok($msg)   { Write-Host "  [ok] $msg" -ForegroundColor Green }
function Wrn($msg)  { Write-Host "  [warn] $msg" -ForegroundColor Yellow }

# ── 0) 端口占用检查 ──
$inUse = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue
if ($inUse) { Wrn "端口 $Port 已被占用，可能是服务已在运行（无需重复启动）。" ; exit 0 }

# ── 1) 确保 Ubuntu WSL 的 dockerd 运行 ──
Step '1/5 确保 WSL Docker 引擎运行…'
$dockerdUp = wsl -d $WslDist -u root -- sh -c "pgrep -x dockerd >/dev/null && echo UP || echo DOWN" 2>$null | Select-Object -Last 1
if ($dockerdUp -ne 'UP') {
    wsl -d $WslDist -u root -- sh -c "mkdir -p /var/log/docker && nohup dockerd --host=unix:///var/run/docker.sock --host=tcp://0.0.0.0:2375 >/var/log/docker/dockerd.log 2>&1 &"
    Start-Sleep -Seconds 8
}
wsl -d $WslDist -u root -- docker info 2>$null | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'WSL Docker 引擎未能启动，请检查 wsl -d Ubuntu -- docker info' }
Ok 'WSL Docker 引擎就绪'

# ── 2) 拉起中间件栈（compose up -d）──
Step '2/5 拉起中间件栈（PG / MinIO / OpenSearch / Kafka）…'
wsl -d $WslDist -u root -- sh -c "cd '$WslDeploy' && docker compose up -d 2>&1"
if ($LASTEXITCODE -ne 0) { throw 'docker compose up -d 失败，请检查 WSL 内 docker 状态' }
Ok '容器已启动'

# ── 3) 一键初始化（迁移 / 桶 / 索引模板 / 主题，幂等可重跑）──
Step '3/5 初始化中间件（迁移/MinIO/OpenSearch/Kafka 主题）…'
wsl -d $WslDist -u root -- sh -c "cd '$WslDeploy' && bash bootstrap.sh 2>&1"
if ($LASTEXITCODE -ne 0) { throw 'bootstrap.sh 初始化失败，请查看上方输出' }
Ok '初始化完成'

# ── 4) 编译自包含服务（可选 -Rebuild 强制重编）──
Step '4/5 准备自包含集成服务（integration + webui）…'
if (-not (Test-Path $ServerExe) -or $Rebuild) {
    $env:GOFLAGS  = '-mod=mod'
    $env:GOPATH   = Join-Path $Root '.gopath'
    $env:GOMODCACHE = Join-Path $Root '.gopath\pkg\mod'
    $env:GOCACHE  = Join-Path $Root '.gocache'
    $env:GOPROXY  = 'https://goproxy.cn,direct'
    Push-Location $Root
    try { go build -tags 'integration webui' -o $ServerExe ./cmd }
    finally { Pop-Location }
    if ($LASTEXITCODE -ne 0) { throw 'Go 编译失败（go build -tags "integration webui"）' }
}
Ok "二进制就绪：$ServerExe"

# ── 5) 启动服务（宿主进程，连 127.0.0.1 已发布端口）──
Step "5/5 启动服务于 http://localhost:$Port …"
$env:PG_DSN            = 'postgres://agg:agg-secret@127.0.0.1:15432/mailagg'
$env:MINIO_ENDPOINT    = '127.0.0.1:9000'
$env:MINIO_BUCKET      = 'agg-mail'
$env:MINIO_ACCESS_KEY  = 'agg'
$env:MINIO_SECRET_KEY  = 'agg-secret'
$env:MINIO_SECURE      = 'false'
$env:OPENSEARCH_ADDR   = 'https://127.0.0.1:9200'
$env:OPENSEARCH_USER   = 'admin'
$env:OPENSEARCH_PASS   = 'Kp3mQ9@vL2*rT7xA8'
$env:KAFKA_BROKERS     = '127.0.0.1:9092'
$env:HTTP_PORT         = "$Port"
Start-Process -FilePath $ServerExe `
    -RedirectStandardOutput (Join-Path $BinDir 'integ-webui.log') `
    -RedirectStandardError  (Join-Path $BinDir 'integ-webui.err.log') `
    -WindowStyle Hidden

# ── 健康检查 ──
$healthy = $false
for ($i = 0; $i -lt 20; $i++) {
    Start-Sleep -Seconds 2
    try { $h = Invoke-RestMethod -Uri "http://127.0.0.1:$Port/api/health" -TimeoutSec 3; if ($h.ok) { $healthy = $true; break } } catch { }
}
if (-not $healthy) {
    Write-Host '  服务未通过健康检查，最近错误日志：' -ForegroundColor Yellow
    Get-Content (Join-Path $BinDir 'integ-webui.err.log') -Tail 20 -ErrorAction SilentlyContinue
    throw '服务启动失败，请查看上方错误日志'
}
Ok "服务已就绪： http://localhost:$Port  （API/WS 同端口；演示新邮件 POST /api/demo/push）"

