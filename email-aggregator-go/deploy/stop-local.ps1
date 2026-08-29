# =============================================================
# stop-local.ps1 —— 停止邮箱聚合平台服务（保留中间件栈）
# 说明：仅停宿主 Go 服务进程；WSL 中间件容器与数据卷保留，便于下次秒启。
#   如需连中间件一并停掉：  docker compose down   （数据保留）
#   如需完全清空数据：      docker compose down -v
# =============================================================
$exe = Join-Path $PSScriptRoot '..\bin\integ-webui.exe'
Get-Process -Name 'integ-webui' -ErrorAction SilentlyContinue | Stop-Process -Force
Write-Host '邮箱聚合服务已停止（中间件栈保留运行）。'
Write-Host '如需停止中间件： wsl -d Ubuntu -- sh -c "cd /mnt/d/网站全栈项目/项目003/email-aggregator-go/deploy && docker compose down"'
