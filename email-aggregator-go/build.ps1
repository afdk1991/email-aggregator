# 跨平台发布流水线（PowerShell 版，与 build.sh 等价）。
# 为 Windows / macOS / Linux 的 amd64 / arm64 交叉编译自包含单文件二进制并打包。
#
# 用法：
#   pwsh ./build.ps1                 # 全平台
#   pwsh ./build.ps1 windows         # 仅 Windows
#   pwsh ./build.ps1 linux amd64     # 仅 Linux/amd64
param(
  [string[]]$Filter = @()
)

$ErrorActionPreference = 'Stop'
$ROOT = Split-Path -Parent $MyInvocation.MyCommand.Path
$WEB  = Join-Path $ROOT '..' 'email-aggregator-web'
$OUT  = Join-Path $ROOT 'release'

$TARGETS = @(
  @{os='windows'; arch='amd64'},
  @{os='windows'; arch='arm64'},
  @{os='darwin';  arch='amd64'},
  @{os='darwin';  arch='arm64'},
  @{os='linux';   arch='amd64'},
  @{os='linux';   arch='arm64'}
)

if ($Filter.Count -gt 0) {
  $TARGETS = $TARGETS | Where-Object { $Filter -contains $_.os -or $Filter -contains $_.arch }
}

Write-Host '==> [1/4] 构建前端 SPA'
Push-Location $WEB
if (Test-Path node_modules) { npm ci } else { npm install }
npm run build
Pop-Location

Write-Host '==> [2/4] 复制 dist 到后端 webroot/'
Remove-Item -Recurse -Force "$ROOT/webroot/dist" -ErrorAction SilentlyContinue
Copy-Item -Recurse "$WEB/dist" "$ROOT/webroot/dist"

Write-Host '==> [3/4] 交叉编译（CGO_ENABLED=0 -tags webui）'
New-Item -ItemType Directory -Force -Path $OUT | Out-Null
foreach ($t in $TARGETS) {
  $ext = if ($t.os -eq 'windows') { '.exe' } else { '' }
  $bin = "email-aggregator-$($t.os)-$($t.arch)$ext"
  Write-Host "    -> $bin"
  $env:CGO_ENABLED = '0'
  $env:GOOS = $t.os
  $env:GOARCH = $t.arch
  & go build -tags webui -ldflags='-s -w' -o (Join-Path $OUT $bin) (Join-Path $ROOT 'cmd/server')
  if ($LASTEXITCODE -ne 0) { throw "build failed for $($t.os)/$($t.arch)" }
}

Write-Host '==> [4/4] 打包归档'
Push-Location $OUT
Get-ChildItem email-aggregator-* | ForEach-Object {
  if ($_.Name -like '*.exe') {
    Compress-Archive -Force -Path $_.FullName -DestinationPath "$($_.Name -replace '\.exe$','').zip"
    Write-Host "    -> $($_.Name -replace '\.exe$','').zip"
  } else {
    tar -czf "$($_.Name).tar.gz" $_.Name
    Write-Host "    -> $($_.Name).tar.gz"
  }
}
Pop-Location

Write-Host "==> 完成。产物位于 $OUT"
Get-ChildItem $OUT | Format-Table Name, Length
