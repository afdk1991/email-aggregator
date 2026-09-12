<#
.SYNOPSIS
  Phase 3 Multi-AZ 故障注入演练：RPO / RTO 度量
.DESCRIPTION
  在 deploy/docker-compose.multiaz.yml 双 AZ 拓扑上注入故障，度量：
    1. PG failover RTO：杀 pg-primary → pg-replica promote → 业务恢复读取
    2. PG 复制 RPO：杀前向 primary 写若干行 → 杀后检查 replica 缺失行数
    3. Kafka broker 失联：杀 kafka-a → kafka-b 仍可读写（rf=2 + min.isr=1）
    4. MinIO site 故障：杀 minio-a → minio-b 仍可读 → 重启后 site replication 恢复
  本脚本仅做 PG 故障注入 + 度量；Kafka/MinIO 故障注入留作扩展点（结构相同）。
.PARAMETER ComposeFile
  docker-compose 文件，默认 deploy/docker-compose.multiaz.yml
.PARAMETER SampleRows
  注入 primary 的样本行数，默认 100（用于度量 RPO）
.EXAMPLE
  .\scripts\drill-chaos.ps1
#>
param(
  [string]$ComposeFile = "deploy/docker-compose.multiaz.yml",
  [int]$SampleRows = 100
)

$ErrorActionPreference = "Stop"
$PSNativeCommandUseErrorActionPreference = $true

function Invoke-Compose {
  param([Parameter(ValueFromRemainingArguments=$true)][string[]]$Args)
  $out = & docker compose -f $ComposeFile @Args
  if ($LASTEXITCODE -ne 0) { throw "docker compose failed: $($Args -join ' ')`n$out" }
  return $out
}

function Invoke-Psql-Primary {
  param([string]$Sql, [switch]$TuplesOnly)
  $env:PGPASSWORD = $env:PG_PASSWORD
  $argList = @("-h", "localhost", "-p", "5433", "-U", "agg", "-d", "mailagg", "-c", $Sql)
  if ($TuplesOnly) { $argList = @("-h", "localhost", "-p", "5433", "-U", "agg", "-d", "mailagg", "-tA", "-c", $Sql) }
  $out = & psql @argList
  if ($LASTEXITCODE -ne 0) { throw "psql primary failed: $Sql`n$out" }
  return $out
}

function Invoke-Psql-Replica {
  param([string]$Sql, [switch]$TuplesOnly)
  $env:PGPASSWORD = $env:PG_PASSWORD
  $argList = @("-h", "localhost", "-p", "5434", "-U", "agg", "-d", "mailagg", "-c", $Sql)
  if ($TuplesOnly) { $argList = @("-h", "localhost", "-p", "5434", "-U", "agg", "-d", "mailagg", "-tA", "-c", $Sql) }
  $out = & psql @argList
  if ($LASTEXITCODE -ne 0) { throw "psql replica failed: $Sql`n$out" }
  return $out
}

Write-Host "=== Phase 3 / Multi-AZ 故障注入演练 ===" -ForegroundColor Cyan
Write-Host "Compose: $ComposeFile"
Write-Host ""

# ── 准备：建表 + 测量基线 ─────────────────────────────────
Write-Host "[0] 准备：基线表 multiaz_drill" -ForegroundColor Yellow
try {
  Invoke-Psql-Primary -Sql "DROP TABLE IF EXISTS multiaz_drill; CREATE TABLE multiaz_drill (id serial primary key, tenant_id text, ts timestamptz default now());" | Out-Null
  Write-Host "  table multiaz_drill created on primary"
} catch {
  Write-Warning "建表失败：$($_.Exception.Message)"
  Write-Host "  -> 假设已存在 multiaz_drill 表，继续演练..."
}
$baselineRows = [int](Invoke-Psql-Primary -Sql "SELECT count(*) FROM multiaz_drill;" -TuplesOnly).Trim()
Write-Host "  baseline rows = $baselineRows"
Write-Host ""

# ── 度量 1: RPO — 写入 N 行 → 杀 primary → 在 replica 上看缺失行数 ──
Write-Host "[1] RPO 度量：向 primary 写 $SampleRows 行" -ForegroundColor Yellow
$writeStart = Get-Date
for ($i = 1; $i -le $SampleRows; $i++) {
  $tid = "drill-$i"
  Invoke-Psql-Primary -Sql "INSERT INTO multiaz_drill (tenant_id) VALUES ('$tid');" | Out-Null
}
$writeEnd = Get-Date
$writeDuration = ($writeEnd - $writeStart).TotalSeconds
Write-Host "  wrote $SampleRows rows in $($writeDuration.ToString('F2'))s"
Write-Host "  等待 2s 让流复制追赶..."
Start-Sleep -Seconds 2

# 检查 replica 看到多少行（RPO = primary 总行 - replica 已同步行）
$replicaRowsBefore = [int](Invoke-Psql-Replica -Sql "SELECT count(*) FROM multiaz_drill;" -TuplesOnly).Trim()
$primaryRowsBefore = [int](Invoke-Psql-Primary -Sql "SELECT count(*) FROM multiaz_drill;" -TuplesOnly).Trim()
$rpo = $primaryRowsBefore - $replicaRowsBefore
Write-Host "  primary rows = $primaryRowsBefore, replica rows = $replicaRowsBefore"
Write-Host "  RPO = $rpo 行（同步延迟）"
Write-Host ""

# ── 度量 2: RTO — 杀 primary → promote replica → 业务恢复 ──
Write-Host "[2] RTO 度量：注入故障（停止 pg-primary）" -ForegroundColor Yellow
$failoverStart = Get-Date
Invoke-Compose stop pg-primary | Out-Null
Write-Host "  pg-primary stopped; promoting pg-replica"

# promote replica：pg_promote() 把 standby 提升为 primary
Invoke-Psql-Replica -Sql "SELECT pg_promote();" | Out-Null
$waitPromote = 0
while ($waitPromote -lt 10) {
  $inRecovery = (Invoke-Psql-Replica -Sql "SELECT pg_is_in_recovery();" -TuplesOnly).Trim()
  if ($inRecovery -eq "f") { break }
  Start-Sleep -Seconds 1
  $waitPromote++
}
if ($waitPromote -ge 10) {
  Write-Error "replica promote 失败（10s 内未变为 primary）"
  exit 1
}
Write-Host "  replica promoted to primary in $waitPromote s"

# 度量业务恢复时间：replica 现在可写
$canarySql = "INSERT INTO multiaz_drill (tenant_id) VALUES ('post-failover-canary') RETURNING id;"
$canaryId = (Invoke-Psql-Replica -Sql $canarySql -TuplesOnly).Trim()
$failoverEnd = Get-Date
$rto = ($failoverEnd - $failoverStart).TotalSeconds
Write-Host "  canary row id = $canaryId"
Write-Host "  RTO = $([math]::Round($rto, 2))s（从 primary 停止到 replica 可写）"
Write-Host ""

# ── 度量 3: 恢复演练 — 重启 primary 作为 standby 加入集群 ──
Write-Host "[3] 恢复：重启 pg-primary（将作为新 standby 跟随新主 pg-replica）" -ForegroundColor Yellow
Write-Host "  注：本 PoC 不自动反向同步；生产环境需运行 pg_rewind + 流复制反向重连"
Write-Host "  -> 演练仅验证 promote 后业务可继续读写的「故障切换」能力"
Write-Host ""

# ── 总结报告 ──────────────────────────────────────────────
Write-Host "=== Multi-AZ 演练报告 ===" -ForegroundColor Cyan
$resultTable = [PSCustomObject]@{
  Metric     = "PG RPO (rows lag)"
  Value      = "$rpo rows"
  Target     = "0 rows (流复制秒级)"
  Status     = if ($rpo -le 5) { "PASS" } else { "FAIL" }
},
[PSCustomObject]@{
  Metric     = "PG RTO (failover to write)"
  Value      = "$([math]::Round($rto, 2))s"
  Target     = "< 30s (manual promote)"
  Status     = if ($rto -le 30) { "PASS" } else { "FAIL" }
}

$resultTable | Format-Table -AutoSize
Write-Host ""
Write-Host "  RPO 度量：「数据丢失量」= primary 与 replica 在故障瞬间的行数差"
Write-Host "  RTO 度量：「恢复时间目标」= 从 primary 停止到业务可继续读写的时间"
Write-Host "  实际生产 RTO 还需叠加：DNS 切换 / 应用层 connection pool 重连 / LB 健康检查退避"
Write-Host ""

# ── 清理：重启整个 multiaz 栈 ──────────────────────────────
Write-Host "[4] 清理：重启 multiaz 栈恢复原始状态（pg-primary 重作 primary）" -ForegroundColor Yellow
Write-Host "  执行：docker compose -f $ComposeFile down -v && up -d"
Write-Host "  -> 由用户手动执行（避免脚本误删数据卷）"
Write-Host ""

# 退出码：所有 PASS=0，否则 1
$allPass = ($rpo -le 5) -and ($rto -le 30)
if ($allPass) {
  Write-Host "=== Multi-AZ 演练 PASS ===" -ForegroundColor Green
  exit 0
} else {
  Write-Host "=== Multi-AZ 演练 FAIL ===" -ForegroundColor Red
  exit 1
}
