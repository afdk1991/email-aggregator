# =============================================================
# start-local.ps1 —— 邮箱聚合平台 一键本地启动
# -------------------------------------------------------------
# 作用：本机 Docker Desktop 首次启动不稳定，历史脚本改用
#   Ubuntu WSL 内原生 dockerd 作为 Docker 引擎。2026-09-03 起
#   Ubuntu WSL 虚拟磁盘(C:\WSL\Ubuntu\ext4.vhdx)被删除丢失，
#   中间件改由宿主机 Docker Desktop 承载（兼容优先），WSL 路径
#   保留为回退分支。脚本自动探测可用引擎：
#     1) 宿主机 docker CLI 可用（Docker Desktop）→ 直接使用
#     2) 否则回退 Ubuntu WSL 原生 dockerd（历史路径）
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

# ── 从 .env 读取中间件凭证（与 compose/bootstrap 对齐）──
function Get-EnvVar($name, $def) {
    $line = Select-String -Path (Join-Path $Deploy '.env') -Pattern "^$name=" -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($line) { $line.Line -replace "^$name=",'' } else { $def }
}
$PG_USER    = Get-EnvVar 'PG_USER' 'agg'
$PG_PASS    = Get-EnvVar 'PG_PASSWORD' 'agg-secret'
$PG_DB      = Get-EnvVar 'PG_DATABASE' 'mailagg'
$MINIO_USER = Get-EnvVar 'MINIO_ROOT_USER' 'agg'
$MINIO_PASS = Get-EnvVar 'MINIO_ROOT_PASSWORD' 'agg-secret'
$OS_ADMIN   = Get-EnvVar 'OPENSEARCH_INITIAL_ADMIN_PASSWORD' 'Kp3mQ9@vL2*rT7xA8'

function Step($msg) { Write-Host "[$([DateTime]::Now.ToString('HH:mm:ss'))] $msg" -ForegroundColor Cyan }
function Ok($msg)   { Write-Host "  [ok] $msg" -ForegroundColor Green }
function Wrn($msg)  { Write-Host "  [warn] $msg" -ForegroundColor Yellow }

# ── 0) 端口占用检查 ──
$inUse = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue
if ($inUse) { Wrn "端口 $Port 已被占用，可能是服务已在运行（无需重复启动）。" ; exit 0 }

# ── Docker 引擎探测：优先宿主机 Docker Desktop，否则 WSL 原生 dockerd ──
$UseHostDocker = $false
if (Get-Command docker -ErrorAction SilentlyContinue) {
    docker info *> $null
    if ($LASTEXITCODE -eq 0) { $UseHostDocker = $true }
}

# ── 1) 确保 Docker 引擎运行 ──
Step '1/5 确保 Docker 引擎运行…'
if ($UseHostDocker) {
    Ok '使用宿主机 Docker Desktop 引擎（docker CLI 可用）'
} else {
    $dockerdUp = wsl -d $WslDist -u root -- sh -c "pgrep -x dockerd >/dev/null && echo UP || echo DOWN" 2>$null | Select-Object -Last 1
    if ($dockerdUp -ne 'UP') {
        wsl -d $WslDist -u root -- sh -c "mkdir -p /var/log/docker && nohup dockerd --host=unix:///var/run/docker.sock --host=tcp://0.0.0.0:2375 >/var/log/docker/dockerd.log 2>&1 &"
        Start-Sleep -Seconds 8
    }
    wsl -d $WslDist -u root -- docker info 2>$null | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'WSL Docker 引擎未能启动，请先启动 Docker Desktop 或恢复 Ubuntu WSL' }
    Ok 'WSL Docker 引擎就绪'
}

# ── 2) 拉起中间件栈（compose up -d）──
Step '2/5 拉起中间件栈（PG / MinIO / OpenSearch / Kafka）…'
if ($UseHostDocker) {
    Push-Location $Deploy
    try { docker compose up -d 2>&1 } finally { Pop-Location }
    if ($LASTEXITCODE -ne 0) { throw 'docker compose up -d 失败，请检查 Docker Desktop 状态' }
} else {
    wsl -d $WslDist -u root -- sh -c "cd '$WslDeploy' && docker compose up -d 2>&1"
    if ($LASTEXITCODE -ne 0) { throw 'docker compose up -d 失败，请检查 WSL 内 docker 状态' }
}
Ok '容器已启动'

# ── 3) 一键初始化（迁移 / 桶 / 索引模板 / 主题，幂等可重跑）──
Step '3/5 初始化中间件（迁移/MinIO/OpenSearch/Kafka 主题）…'
if ($UseHostDocker) {
    # 宿主机路径：直接经 docker compose exec 执行，避免 WSL bash 的 MSYS 路径转换坑
    # （/migrations 被 Git Bash 误转成 C:/Program Files/Git/... 的问题）
    Push-Location $Deploy
    try {
        # 等 PG 就绪
        $pgReady = $false
        for ($i = 0; $i -lt 60; $i++) {
            docker exec deploy-postgres-1 pg_isready -U $PG_USER *> $null
            if ($LASTEXITCODE -eq 0) { $pgReady = $true; break }
            Start-Sleep -Seconds 1
        }
        if (-not $pgReady) { throw 'PostgreSQL 60s 内未就绪' }
        # 重放迁移（幂等）
        foreach ($m in @('001_init.sql','002_citus_sharding.sql','003_read_flag.sql','004_account_registry.sql','005_account_server_host.sql')) {
            docker compose exec -T postgres psql -U $PG_USER -d $PG_DB -v ON_ERROR_STOP=1 -f "/migrations/$m"
            if ($LASTEXITCODE -ne 0) { throw "迁移 $m 失败" }
        }
        # MinIO 建桶
        docker compose run --rm --entrypoint sh mc -c "mc alias set local http://minio:9000 $MINIO_USER $MINIO_PASS && mc mb --ignore-existing local/agg-mail" 2>&1 | Out-Null
        # OpenSearch 索引模板
        $osJson = '{"index_patterns":["mail-*"],"template":{"settings":{"number_of_shards":1,"number_of_replicas":0},"mappings":{"properties":{"accountId":{"type":"keyword"},"subject":{"type":"text"},"from":{"type":"keyword"},"bodyText":{"type":"text"},"internalDate":{"type":"date"}}}}}'
        docker compose exec -i opensearch curl -fsS -u "admin:$OS_ADMIN" -k -X PUT "https://localhost:9200/_index_template/mail" -H 'Content-Type: application/json' -d $osJson *> $null
        # Kafka 主题
        foreach ($t in @('sync-tasks','mail-ingested','mail-index','notifications','audit')) {
            docker compose exec -T kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server kafka:9092 --create --if-not-exists --topic $t --partitions 1 --replication-factor 1 2>&1 | Out-Null
        }
    } finally { Pop-Location }
    Ok '初始化完成（宿主机 Docker）'
} else {
    wsl -d $WslDist -u root -- sh -c "cd '$WslDeploy' && bash bootstrap.sh 2>&1"
    if ($LASTEXITCODE -ne 0) { throw 'bootstrap.sh 初始化失败，请查看上方输出' }
    Ok '初始化完成'
}

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
$env:PG_DSN            = "postgres://${PG_USER}:${PG_PASS}@127.0.0.1:15432/${PG_DB}"
$env:MINIO_ENDPOINT    = '127.0.0.1:9000'
$env:MINIO_BUCKET      = 'agg-mail'
$env:MINIO_ACCESS_KEY  = $MINIO_USER
$env:MINIO_SECRET_KEY  = $MINIO_PASS
$env:MINIO_SECURE      = 'false'
$env:OPENSEARCH_ADDR   = 'https://127.0.0.1:9200'
$env:OPENSEARCH_USER   = 'admin'
$env:OPENSEARCH_PASS   = $OS_ADMIN
$env:KAFKA_BROKERS     = '127.0.0.1:9092'
# Kafka advertised 通告容器内网名 deploy-kafka-1，宿主机无法解析 —— 强制拨号回环，否则 mail-ingested 事件生产失败、
# 摄取 worker 收不到事件导致邮件永不落库（真实联调踩坑：正文一直为空即此因）。
$env:KAFKA_DIAL_ADDR   = '127.0.0.1:9092'
# 持久化 KEK（信封加密凭据用）。KMS_KEYRING_FILE 缺省时 newCredentialVault 退回「内存随机 KEK」，
# 进程重启后即无法拆封历史凭据（cipher: message authentication failed）。
# NewLocalKmsFromFile 在文件不存在时自动生成初始密钥并落盘，故只需指向固定路径。
$env:KMS_KEYRING_FILE  = Join-Path $BinDir 'kms-keyring.json'
$env:HTTP_PORT         = "$Port"
# 本地联调需要「模拟收信」端点（/api/demo/push 无鉴权、会写库+推 WS，集成形态下默认关闭）。
# 与 EdgeOne 云函数版 ENABLE_DEMO 同名同语义 —— 生产环境切勿设置。
$env:ENABLE_DEMO       = 'true'
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
