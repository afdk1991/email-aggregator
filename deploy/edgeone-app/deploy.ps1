# ============================================================
#  一键重新部署「邮箱聚合平台」到 EdgeOne Makers（Windows / PowerShell）
# ============================================================
#
#  本脚本只是 scripts/deploy-edgeone.mjs 的薄壳。
#  发布逻辑**只有一处实现**（那个 Node 脚本），本地 Windows/macOS/Linux 与 CI
#  走完全相同的代码路径，只有"谁来调用"不同。保留本文件的原因很单纯：
#  Windows 上习惯右键/双击运行 .ps1，比手打 node 命令顺手。
#
#  发布器依次完成：
#    ① 版本门禁（version.json → 全部落点，漂移即中止）
#    ② 前端构建（npm run build）
#    ③ 产物同步（五处目标 + 逐文件 SHA-256 复验，并清理陈旧残留）
#    ④ 项目绑定（edgeone-project.json → .edgeone/project.json，防止 CLI 另建项目）
#    ⑤ 本地冒烟（演示形态 + 生产形态各一套）→ 部署
#    ⑥ 线上全接口验证（-VerifyLive）
#
#  用法：
#    powershell -ExecutionPolicy Bypass -File .\deploy.ps1
#    powershell -ExecutionPolicy Bypass -File .\deploy.ps1 -ProjectName my-project
#    powershell -ExecutionPolicy Bypass -File .\deploy.ps1 -SkipBuild      # 复用已有 dist
#    powershell -ExecutionPolicy Bypass -File .\deploy.ps1 -DryRun         # 演练，不真正部署
#    powershell -ExecutionPolicy Bypass -File .\deploy.ps1 -VerifyLive     # 部署后跑线上验证
#    $env:EDGEONE_TOKEN="xxx"; powershell -ExecutionPolicy Bypass -File .\deploy.ps1
#
#  注意：本文件含中文，**必须带 UTF-8 BOM**，否则 Windows PowerShell 5.1 会按
#  ANSI(GBK) 解码并解析失败（见 scripts/check-ps1-encoding.ps1）。
# ============================================================
param(
    [string]$ProjectName = "",
    [switch]$SkipBuild,
    [switch]$DryRun,
    [switch]$VerifyLive
)

$ErrorActionPreference = 'Stop'

$Here = Split-Path -Parent $MyInvocation.MyCommand.Path
$Root = Resolve-Path (Join-Path $Here '..\..')
$Deployer = Join-Path $Root 'scripts\deploy-edgeone.mjs'

if (-not (Test-Path -LiteralPath $Deployer)) {
    Write-Host "[FAIL] 找不到发布器：$Deployer" -ForegroundColor Red
    exit 2
}

$nodeArgs = @($Deployer)
if ($ProjectName) { $nodeArgs += @('--project', $ProjectName) }
if ($SkipBuild) { $nodeArgs += '--skip-build' }
if ($DryRun) { $nodeArgs += '--dry-run' }
if ($VerifyLive) { $nodeArgs += '--verify-live' }

Push-Location $Root
try {
    & node @nodeArgs
    $code = $LASTEXITCODE
}
finally {
    Pop-Location
}

if ($code -ne 0) { Write-Host "[FAIL] 发布未完成（exit $code）" -ForegroundColor Red }
exit $code
