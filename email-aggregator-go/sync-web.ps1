<#
.SYNOPSIS
    同步前端构建产物到 Go 内置资源目录（webroot/dist），并做 SHA-256 逐文件校验。

.DESCRIPTION
    背景：`webroot/dist` 既是构建产物（被 .gitignore 忽略），又是 `//go:build webui` 的 embed 输入。
    `build.ps1` / `build.sh` 的第 [2/4] 步会同步它 —— 但**直接 `go build -tags webui` 做快速验证时会绕过
    那个脚本**，而目录里有旧文件照样能编译通过，于是产物静默漂移。曾实际发生两次：
      · 「内嵌 SPA 滞后一整版」：云上已是新 UI，单二进制仍吐出旧版；
      · 「漏同步 CSS」：JS 更新了、CSS 没跟上，单二进制吐出无样式页面。

    本脚本把这一环节独立出来，提供两种用途：
      1. 默认：同步 `../email-aggregator-web/dist` → `./webroot/dist`
      2. `-Verify`：只校验一致性、不做任何修改，不一致时以非零码退出
         （适合挂在构建前 / CI 里，把「静默漂移」从"看不出来"变成"显式失败"）

    注意：本机用 Windows PowerShell 5.1 运行本脚本时，**文件必须带 UTF-8 BOM**。
    PS 5.1 对无 BOM 文件按系统 ANSI(GBK) 解码，中文注释会乱码并吞掉后随的引号，
    导致 "字符串缺少终止符" 之类的解析错误。仓库根 `scripts/check-ps1-encoding.ps1` 负责巡检。

.PARAMETER Verify
    仅校验，不同步。一致返回 0，不一致返回 1。

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File .\sync-web.ps1
    powershell -ExecutionPolicy Bypass -File .\sync-web.ps1 -Verify
#>
[CmdletBinding()]
param(
    [switch]$Verify
)

$ErrorActionPreference = 'Stop'

$ROOT = Split-Path -Parent $MyInvocation.MyCommand.Path
$SRC  = Join-Path $ROOT '..\email-aggregator-web\dist'
$DST  = Join-Path $ROOT 'webroot\dist'

# 比较两棵目录树：返回差异描述数组（空数组 = 完全一致）
function Compare-Assets {
    param([string]$A, [string]$B)

    $diffs = @()

    $aDir = Join-Path $A 'assets'
    $bDir = Join-Path $B 'assets'
    $aAssets = @()
    $bAssets = @()
    if (Test-Path $aDir) { $aAssets = @(Get-ChildItem $aDir -File) }
    if (Test-Path $bDir) { $bAssets = @(Get-ChildItem $bDir -File) }

    $aNames = @($aAssets | ForEach-Object { $_.Name })
    $bNames = @($bAssets | ForEach-Object { $_.Name })

    foreach ($n in $aNames) {
        if ($bNames -notcontains $n) { $diffs += "源有而内嵌目录缺：assets/$n" }
    }
    foreach ($n in $bNames) {
        if ($aNames -notcontains $n) { $diffs += "内嵌目录有而源缺（陈旧残留）：assets/$n" }
    }

    # 同名文件逐一比对 —— JS 与 CSS **都要验**，只看文件数或只看 JS 会漏掉 CSS 漂移
    foreach ($f in $aAssets) {
        $peer = $bAssets | Where-Object { $_.Name -eq $f.Name } | Select-Object -First 1
        if (-not $peer) { continue }
        $ha = (Get-FileHash -LiteralPath $f.FullName -Algorithm SHA256).Hash
        $hb = (Get-FileHash -LiteralPath $peer.FullName -Algorithm SHA256).Hash
        if ($ha -ne $hb) {
            $diffs += "内容不一致：assets/$($f.Name)  SRC=$($ha.Substring(0,12))...  DST=$($hb.Substring(0,12))..."
        }
    }

    $ia = Join-Path $A 'index.html'
    $ib = Join-Path $B 'index.html'
    $ea = Test-Path $ia
    $eb = Test-Path $ib
    if ($ea -and $eb) {
        $ha = (Get-FileHash -LiteralPath $ia -Algorithm SHA256).Hash
        $hb = (Get-FileHash -LiteralPath $ib -Algorithm SHA256).Hash
        if ($ha -ne $hb) { $diffs += "内容不一致：index.html" }
    }
    elseif ($ea) { $diffs += "内嵌目录缺少 index.html" }
    elseif ($eb) { $diffs += "源缺少 index.html" }

    return $diffs
}

# 增量同步：递归覆盖同名文件 + 清除「目标中有、源中没有」的陈旧残留。
# 刻意**不用**「整目录删除后再拷贝」——那样一旦中途失败（例如回收站不可用、
# 脚本被中断），embed 输入目录会整体消失，把一次「产物不一致」升级成「根本没法构建」。
function Sync-Assets {
    param([string]$From, [string]$To)

    if (-not (Test-Path $To)) { New-Item -ItemType Directory -Force -Path $To | Out-Null }

    $fromTop = @(Get-ChildItem -LiteralPath $From -Force)
    foreach ($item in $fromTop) {
        $dest = Join-Path $To $item.Name
        if ($item.PSIsContainer) {
            if (-not (Test-Path $dest)) { New-Item -ItemType Directory -Force -Path $dest | Out-Null }
            Sync-Assets -From $item.FullName -To $dest
        }
        else {
            Copy-Item -LiteralPath $item.FullName -Destination $dest -Force
        }
    }

    $fromNames = @($fromTop | ForEach-Object { $_.Name })
    foreach ($item in @(Get-ChildItem -LiteralPath $To -Force)) {
        if ($fromNames -notcontains $item.Name) {
            Write-Host "    清除陈旧残留: $($item.Name)" -ForegroundColor Yellow
            Remove-Item -LiteralPath $item.FullName -Recurse -Force
        }
    }
}

if (-not (Test-Path $SRC)) {
    Write-Host "x 前端构建产物不存在：$SRC" -ForegroundColor Red
    Write-Host "  请先在 email-aggregator-web 下执行：npm run build" -ForegroundColor Yellow
    exit 1
}

# ---------------- 校验模式（只读，不修改任何文件） ----------------
if ($Verify) {
    Write-Host '==> 校验 webroot/dist 与前端构建产物一致性'
    if (-not (Test-Path $DST)) {
        Write-Host "x 内嵌目录不存在：$DST" -ForegroundColor Red
        exit 1
    }
    $diffs = @(Compare-Assets -A $SRC -B $DST)
    if ($diffs.Count -eq 0) {
        Write-Host 'OK 一致（assets/* 与 index.html 的 SHA-256 全部匹配）' -ForegroundColor Green
        exit 0
    }
    Write-Host "x 检测到 $($diffs.Count) 处不一致：" -ForegroundColor Red
    foreach ($d in $diffs) { Write-Host "    $d" }
    Write-Host ''
    Write-Host '  这是「内嵌 SPA 滞后」的典型症状。执行不带 -Verify 的本脚本即可修复。' -ForegroundColor Yellow
    exit 1
}

# ---------------- 同步模式 ----------------
Write-Host '==> [1/2] 同步 dist -> webroot/dist'
Sync-Assets -From $SRC -To $DST

Write-Host '==> [2/2] SHA-256 校验'
$diffs = @(Compare-Assets -A $SRC -B $DST)
if ($diffs.Count -ne 0) {
    Write-Host "x 同步后仍不一致（$($diffs.Count) 处）：" -ForegroundColor Red
    foreach ($d in $diffs) { Write-Host "    $d" }
    exit 1
}

$js  = Get-ChildItem -LiteralPath (Join-Path $DST 'assets') -Filter '*.js'  | Select-Object -First 1
$css = Get-ChildItem -LiteralPath (Join-Path $DST 'assets') -Filter '*.css' | Select-Object -First 1
Write-Host 'OK 同步完成，校验通过' -ForegroundColor Green
if ($js)  { Write-Host ('    JS   ' + $js.Name  + '  ' + (Get-FileHash -LiteralPath $js.FullName  -Algorithm SHA256).Hash) }
if ($css) { Write-Host ('    CSS  ' + $css.Name + '  ' + (Get-FileHash -LiteralPath $css.FullName -Algorithm SHA256).Hash) }
exit 0
