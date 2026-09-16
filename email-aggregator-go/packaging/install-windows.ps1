# 以"管理员"身份运行 PowerShell，将自包含二进制注册为 Windows 服务并启动。
# 服务名：EmailAggregator；二进制路径按实际放置位置修改。

$bin  = "C:\Program Files\EmailAggregator\email-aggregator-windows-amd64.exe"
$name = "EmailAggregator"

if (-not (Test-Path $bin)) {
  Write-Error "未找到二进制：$bin —— 请先解压 release 并把 exe 放到该路径。"
  exit 1
}

# sc.exe 的 key= value 语法中，等号后必须有空格
sc.exe create $name binPath= "`"$bin`"" start= auto DisplayName= "Email Aggregator"
sc.exe description $name "邮箱聚合平台（自包含单文件服务，含前端 SPA）"
sc.exe start $name

Write-Host "已注册并启动服务 $name。浏览器访问 http://localhost:8080"
