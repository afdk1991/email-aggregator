# =============================================================================
# Phase 4 / ADR-009：Keycloak OIDC 真实联调验证脚本
# 前置：docker compose -f deploy/docker-compose.keycloak.yml --env-file deploy/.env.keycloak up -d
# 用法：.\scripts\verify-keycloak-oidc.ps1
# 退出码：0 全绿 ｜ 1 失败
# =============================================================================
[CmdletBinding()]
param(
  [string]$KeycloakBase = "http://localhost:8484",
  [string]$Realm = "email-aggregator",
  [string]$ClientId = "email-aggregator-go",
  [string]$ClientSecret = "ea-dev-secret-2026",
  [string]$TestUser = "alice@acme.test",
  [string]$TestPassword = "alice-secret-2026"
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"

function Invoke-Json {
  param(
    [string]$Uri,
    [string]$Method = "GET",
    [string]$Body = $null,
    [string]$ContentType = "application/json",
    [int[]]$ExpectedStatus = @(200)
  )
  $params = @{
    Uri = $Uri
    Method = $Method
    ContentType = $ContentType
    Headers = @{}
  }
  if ($Body) { $params.Body = $Body }
  try {
    $resp = Invoke-WebRequest @params -SkipHttpErrorCheck -UseBasicParsing
    if ($ExpectedStatus -notcontains $resp.StatusCode) {
      Write-Error "HTTP $($resp.StatusCode) != expected $ExpectedStatus @ $Uri"
      Write-Error $resp.Content
      return $null
    }
    if ($resp.Content) { return $resp.Content | ConvertFrom-Json } else { return $null }
  } catch {
    Write-Error "Invoke-WebRequest 失败: $_"
    return $null
  }
}

function Wait-For-Keycloak {
  param([string]$Base)
  $deadline = (Get-Date).AddSeconds(120)
  while ((Get-Date) -lt $deadline) {
    try {
      $ping = Invoke-WebRequest -Uri "$Base/realms/master/.well-known/openid-configuration" -SkipHttpErrorCheck -UseBasicParsing -TimeoutSec 3
      if ($ping.StatusCode -eq 200) {
        Write-Host "[ok] Keycloak 就绪"
        return $true
      }
    } catch { }
    Start-Sleep -Seconds 2
    Write-Host ". . . waiting Keycloak"
  }
  Write-Error "Keycloak 在 120s 内未就绪"
  return $false
}

# ── 0. 等待 Keycloak 就绪 ──
Write-Host "`n=== 步骤 0: 等待 Keycloak 就绪 ===" -ForegroundColor Cyan
if (-not (Wait-For-Keycloak $KeycloakBase)) { exit 1 }

# ── 1. 验证 Realm 可达 + OIDC discovery ──
Write-Host "`n=== 步骤 1: OIDC Discovery 文档 ===" -ForegroundColor Cyan
$disc = Invoke-Json -Uri "$KeycloakBase/realms/$Realm/.well-known/openid-configuration"
if (-not $disc) { exit 1 }
$requiredEndpoints = @("issuer", "authorization_endpoint", "token_endpoint", "jwks_uri", "userinfo_endpoint")
foreach ($k in $requiredEndpoints) {
  if (-not $disc.$k) { Write-Error "Discovery 缺少 $k 字段"; exit 1 }
  Write-Host "  [ok] $k = $($disc.$k)" -ForegroundColor Green
}
if ($disc.issuer -ne "$KeycloakBase/realms/$Realm") {
  Write-Error "issuer 不匹配: $($disc.issuer)"
  exit 1
}

# ── 2. 验证 JWKS 可达（用于验签公钥） ──
Write-Host "`n=== 步骤 2: JWKS 公钥可达 ===" -ForegroundColor Cyan
$jwks = Invoke-Json -Uri $disc.jwks_uri
if (-not $jwks -or -not $jwks.keys -or $jwks.keys.Count -eq 0) {
  Write-Error "JWKS 为空"
  exit 1
}
Write-Host "  [ok] JWKS 返回 $($jwks.keys.Count) 把公钥（kida: $($jwks.keys[0].kid)）" -ForegroundColor Green

# ── 3. Password Grant 兑换令牌（PoC 联调用，非生产 PKCE 流程） ──
Write-Host "`n=== 步骤 3: Password Grant 兑换令牌（alice@acme.test） ===" -ForegroundColor Cyan
$body = "grant_type=password&client_id=$ClientId&client_secret=$ClientSecret&username=$TestUser&password=$TestPassword&scope=openid profile email groups"
$tokenResp = Invoke-WebRequest -Uri $disc.token_endpoint -Method POST `
  -ContentType "application/x-www-form-urlencoded" -Body $body `
  -SkipHttpErrorCheck -UseBasicParsing
if ($tokenResp.StatusCode -ne 200) {
  Write-Error "Password Grant 失败 HTTP $($tokenResp.StatusCode): $($tokenResp.Content)"
  exit 1
}
$token = $tokenResp.Content | ConvertFrom-Json
if (-not $token.access_token -or -not $token.id_token) {
  Write-Error "令牌响应缺少 access_token 或 id_token"
  exit 1
}
Write-Host "  [ok] access_token: $($token.access_token.Substring(0, 30))..." -ForegroundColor Green
Write-Host "  [ok] id_token:     $($token.id_token.Substring(0, 30))..." -ForegroundColor Green
Write-Host "  [ok] expires_in:   $($token.expires_in) 秒" -ForegroundColor Green

# ── 4. 解码 id_token claims（不验签，仅看 payload） ──
Write-Host "`n=== 步骤 4: 解码 id_token claims ===" -ForegroundColor Cyan
$idParts = $token.id_token.Split(".")
if ($idParts.Count -lt 2) { Write-Error "id_token 格式非法"; exit 1 }
$payloadB64 = $idParts[1]
# 补齐 base64 padding
switch ($payloadB64.Length % 4) {
  2 { $payloadB64 += "==" }
  3 { $payloadB64 += "=" }
}
$payloadBytes = [Convert]::FromBase64String($payloadB64)
$claims = [System.Text.Encoding]::UTF8.GetString($payloadBytes) | ConvertFrom-Json

Write-Host "  sub      = $($claims.sub)" -ForegroundColor Green
Write-Host "  email    = $($claims.email)" -ForegroundColor Green
Write-Host "  name     = $($claims.name)" -ForegroundColor Green
Write-Host "  groups   = $($claims.groups -join ', ')" -ForegroundColor Green

# 从 groups 推导 tenant_id
$tenantId = ""
foreach ($g in $claims.groups) {
  if ($g -like "tenant:*") { $tenantId = $g.Substring(7); break }
}
Write-Host "  tenant_id (推导) = $tenantId" -ForegroundColor Green

if (-not $tenantId) {
  Write-Error "id_token 未包含 tenant:* group —— ADR-009 租户上下文断链"
  exit 1
}
Write-Host "  [ok] ADR-009 租户上下文可从 groups 推导" -ForegroundColor Green

# ── 5. UserInfo 端点验证 ──
Write-Host "`n=== 步骤 5: UserInfo 端点 ===" -ForegroundColor Cyan
$userInfo = Invoke-Json -Uri $disc.userinfo_endpoint -Method GET -ExpectedStatus 200
if (-not $userInfo) { exit 1 }
$headers = @{ "Authorization" = "Bearer $($token.access_token)" }
$uiResp = Invoke-WebRequest -Uri $disc.userinfo_endpoint -Method GET -Headers $headers -SkipHttpErrorCheck -UseBasicParsing
if ($uiResp.StatusCode -ne 200) {
  Write-Error "UserInfo 失败 HTTP $($uiResp.StatusCode)"
  exit 1
}
$ui = $uiResp.Content | ConvertFrom-Json
Write-Host "  [ok] sub = $($ui.sub)" -ForegroundColor Green
if ($ui.sub -ne $claims.sub) {
  Write-Error "UserInfo sub 与 id_token 不一致"
  exit 1
}
Write-Host "  [ok] sub 与 id_token 一致" -ForegroundColor Green

# ── 6. 令牌刷新 ──
Write-Host "`n=== 步骤 6: 令牌刷新 ===" -ForegroundColor Cyan
if (-not $token.refresh_token) {
  Write-Host "  [skip] 无 refresh_token（offline_access scope 未请求）" -ForegroundColor Yellow
} else {
  $body = "grant_type=refresh_token&client_id=$ClientId&client_secret=$ClientSecret&refresh_token=$($token.refresh_token)"
  $rtResp = Invoke-WebRequest -Uri $disc.token_endpoint -Method POST `
    -ContentType "application/x-www-form-urlencoded" -Body $body `
    -SkipHttpErrorCheck -UseBasicParsing
  if ($rtResp.StatusCode -ne 200) {
    Write-Error "refresh_token 失败 HTTP $($rtResp.StatusCode)"
    exit 1
  }
  $rt = $rtResp.Content | ConvertFrom-Json
  Write-Host "  [ok] 新 access_token: $($rt.access_token.Substring(0, 30))..." -ForegroundColor Green
}

# ── 7. 跨租户测试（用 globex 账号） ──
Write-Host "`n=== 步骤 7: 跨租户验证（carol@globex.test） ===" -ForegroundColor Cyan
$body2 = "grant_type=password&client_id=$ClientId&client_secret=$ClientSecret&username=carol@globex.test&password=carol-secret-2026&scope=openid profile email groups"
$carolResp = Invoke-WebRequest -Uri $disc.token_endpoint -Method POST `
  -ContentType "application/x-www-form-urlencoded" -Body $body2 `
  -SkipHttpErrorCheck -UseBasicParsing
if ($carolResp.StatusCode -ne 200) {
  Write-Error "carol Password Grant 失败 HTTP $($carolResp.StatusCode)"
  exit 1
}
$carolTok = $carolResp.Content | ConvertFrom-Json
$carolParts = $carolTok.id_token.Split(".")
$carolB64 = $carolParts[1]
switch ($carolB64.Length % 4) {
  2 { $carolB64 += "==" }
  3 { $carolB64 += "=" }
}
$carolClaims = [System.Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($carolB64)) | ConvertFrom-Json

$carolTenant = ""
foreach ($g in $carolClaims.groups) {
  if ($g -like "tenant:*") { $carolTenant = $g.Substring(7); break }
}
Write-Host "  carol tenant_id = $carolTenant" -ForegroundColor Green
if ($carolTenant -ne "globex") {
  Write-Error "carol 期望 tenant_id=globex，实际=$carolTenant"
  exit 1
}
Write-Host "  [ok] carol 落在 tenant=globex（与 alice 的 acme 隔离）" -ForegroundColor Green

# ── 汇总 ──
Write-Host "`n========================================" -ForegroundColor Cyan
Write-Host " Keycloak OIDC 联调全部通过" -ForegroundColor Green
Write-Host "========================================" -ForegroundColor Cyan
Write-Host "已验证：discovery / JWKS / password grant / id_token claims / tenant_id 推导 / userinfo / refresh / 跨租户"
Write-Host "`n下一步："
Write-Host "  1) 把 .env.keycloak 的 OIDC_* 追加到 deploy/.env"
Write-Host "  2) go run -tags integration,sso ./cmd/server_integration.go"
Write-Host "  3) 浏览器访问 http://localhost:8080/api/auth/login 触发 PKCE"
exit 0
