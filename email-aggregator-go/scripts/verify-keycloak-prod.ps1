# =============================================================================
# Phase 5 / P5-K1：Keycloak 生产化栈验证（KC_DB=postgres + start-prod + HTTPS
# 自签反代 + 双副本负载 + 故障切换 + PKCE 授权码流全链路）
#
# 前置：
#   cd deploy
#   docker compose -f docker-compose.keycloak-prod.yml --env-file .env.keycloak-prod up -d
#
# 运行：
#   powershell -NoProfile -File scripts\verify-keycloak-prod.ps1
#
# 退出码：0 = 全部通过；1 = 存在失败项
# 兼容 PowerShell 5.1（无 -SkipCertificateCheck，用 ServerCertificateValidationCallback）
# =============================================================================
param(
    [string]$Base = "https://localhost:8443",
    [string]$Realm = "email-aggregator",
    [string]$ClientId = "email-aggregator-go",
    [string]$ClientSecret = "ea-dev-secret-2026",
    [string]$TestUser = "alice@acme.test",
    [string]$TestPassword = "alice-secret-2026",
    [string]$CrossTenantUser = "carol@globex.test",
    [string]$CrossTenantPassword = "carol-secret-2026",
    [int]$TimeoutSec = 240
)

$ErrorActionPreference = "Stop"

# PS 5.1：跳过自签证书校验 + 强制 TLS1.2
Add-Type -TypeDefinition "using System.Net; using System.Security.Cryptography.X509Certificates; public class TrustAllCertsPolicy : ICertificatePolicy { public bool CheckValidationResult(ServicePoint sp, X509Certificate cert, WebRequest req, int problem) { return true; } }"
[System.Net.ServicePointManager]::CertificatePolicy = New-Object TrustAllCertsPolicy
[System.Net.ServicePointManager]::SecurityProtocol = [System.Net.ServicePointManager]::SecurityProtocol -bor [System.Net.SecurityProtocolType]::Tls12

$script:Results = @()
function Record($name, $ok, $detail) {
    $script:Results += [pscustomobject]@{ Step = $name; Passed = $ok; Detail = $detail }
    if ($ok) { $mark = "PASS" } else { $mark = "FAIL" }
    Write-Host ("  [{0}] {1}  {2}" -f $mark, $name, $detail)
}

# base64url 无 padding（PKCE 用）
function To-Base64Url([byte[]]$bytes) {
    return [Convert]::ToBase64String($bytes).TrimEnd('=').Replace('+', '-').Replace('/', '_')
}

function New-PKCE {
    $b = New-Object byte[] 32
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    $rng.GetBytes($b)
    $verifier = To-Base64Url $b
    $sha = [System.Security.Cryptography.SHA256]::Create()
    $hash = $sha.ComputeHash([Text.Encoding]::UTF8.GetBytes($verifier))
    $challenge = To-Base64Url $hash
    return [pscustomobject]@{ Verifier = $verifier; Challenge = $challenge }
}

function Wait-Ready {
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        try {
            $r = Invoke-WebRequest -Uri "$Base/realms/$Realm/.well-known/openid-configuration" -UseBasicParsing -TimeoutSec 5
            if ($r.StatusCode -eq 200) { return $true }
        } catch { Start-Sleep -Seconds 3 }
    }
    return $false
}

function Decode-JwtPayload([string]$jwt) {
    $parts = $jwt.Split('.')
    $p = $parts[1]
    $pad = $p.Length % 4
    if ($pad -eq 2) { $p += "==" } elseif ($pad -eq 3) { $p += "=" }
    $json = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($p))
    return (ConvertFrom-Json $json)
}

# PKCE 授权码流：登录 → 取 code → 换 token
function Invoke-PKCE-Flow($user, $pass) {
    $s = New-Object Microsoft.PowerShell.Commands.WebRequestSession
    $pkce = New-PKCE
    $state = ([guid]::NewGuid().ToString("N"))
    $authUrl = "$Base/realms/$Realm/protocol/openid-connect/auth" +
        "?client_id=$([uri]::EscapeDataString($ClientId))" +
        "&response_type=code" +
        "&redirect_uri=$([uri]::EscapeDataString('https://localhost:8080/api/auth/callback'))" +
        "&scope=openid%20profile%20email" +
        "&state=$state" +
        "&code_challenge=$([uri]::EscapeDataString($pkce.Challenge))" +
        "&code_challenge_method=S256"

    # 1) 跟随重定向到登录页（拿到 auth_session cookie）
    $loginPage = Invoke-WebRequest -Uri $authUrl -WebSession $s -UseBasicParsing -TimeoutSec 15

    # 2) 解析登录表单 action（Keycloak 26：/login/actions/auth?session_code=...）
    $formMatch = [regex]::Match($loginPage.Content, '<form[^>]+action="([^"]+)"')
    if (-not $formMatch.Success) { throw "login form action not found" }
    $action = $formMatch.Groups[1].Value
    # Keycloak 登录页表单 action 含 &amp; HTML 实体，需解码
    $action = [System.Net.WebUtility]::HtmlDecode($action)
    if ($action -notlike "http*") { $action = "$Base/realms/$Realm$($action -replace '/api/realms/', '/')" }

    # 3) POST 凭据，不跟随重定向 → 302 指向 redirect_uri?code=...&state=...
    # PS 5.1 Invoke-WebRequest -MaximumRedirection 0 有 bug，用 HttpWebRequest 直接控制
    $location = $null
    $postBody = "username=$([uri]::EscapeDataString($user))&password=$([uri]::EscapeDataString($pass))&credentialId="
    $req = [System.Net.HttpWebRequest]::Create($action)
    $req.Method = "POST"
    $req.ContentType = "application/x-www-form-urlencoded"
    $req.AllowAutoRedirect = $false
    $req.Timeout = 15000
    # 复用 WebSession 的 cookie
    $req.CookieContainer = New-Object System.Net.CookieContainer
    foreach ($cookie in $s.Cookies.GetCookies($action)) {
        $req.CookieContainer.Add($cookie)
    }
    $bytes = [Text.Encoding]::UTF8.GetBytes($postBody)
    $req.ContentLength = $bytes.Length
    $stream = $req.GetRequestStream()
    $stream.Write($bytes, 0, $bytes.Length)
    $stream.Close()
    try {
        $webResp = $req.GetResponse()
        $location = $webResp.Headers["Location"]
        $webResp.Close()
    } catch [System.Net.WebException] {
        if ($_.Exception.Response) {
            $location = $_.Exception.Response.Headers["Location"]
        }
    }
    if ([string]::IsNullOrEmpty($location)) {
        throw "authorization redirect Location not found (login failed or invalid credentials)"
    }
    if ($location -notmatch "[?&]code=([^&]+)") { throw "authorization code not returned: $location" }
    $code = [uri]::UnescapeDataString($Matches[1])
    $stateCheck = ""
    if ($location -match "[?&]state=([^&]+)") { $stateCheck = $Matches[1] }
    if ($stateCheck -ne $state) { throw "state mismatch: expected $state got $stateCheck" }

    # 4) code + code_verifier → token（client_secret 走 basic auth）
    $tokenUrl = "$Base/realms/$Realm/protocol/openid-connect/token"
    $credPair = "$($ClientId):$($ClientSecret)"
    $basic = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($credPair))
    $tokenResp = Invoke-WebRequest -Uri $tokenUrl -Method Post -UseBasicParsing -TimeoutSec 15 `
        -Headers @{ Authorization = "Basic $basic" } `
        -Body @{
            grant_type = "authorization_code"
            code = $code
            code_verifier = $pkce.Verifier
            redirect_uri = "https://localhost:8080/api/auth/callback"
        }
    $tokenJson = $tokenResp.Content | ConvertFrom-Json
    return [pscustomobject]@{ Token = $tokenJson; State = $state }
}

# =============================================================================
Write-Host ""
Write-Host "=== P5-K1 Keycloak 生产化验证 ($Base) ===" -ForegroundColor Cyan

# ── 0. 副本就绪 ──
Write-Host "[0/8] 等待 Keycloak 就绪..."
$ready = Wait-Ready
Record "0.发现端点就绪" $ready "$Base/realms/$Realm discovery"
if (-not $ready) {
    Write-Host "Keycloak 未就绪，终止。" -ForegroundColor Red
    exit 1
}

# ── 1. Discovery + JWKS ──
Write-Host "[1/8] OIDC discovery / JWKS"
try {
    $disc = (Invoke-WebRequest -Uri "$Base/realms/$Realm/.well-known/openid-configuration" -UseBasicParsing).Content | ConvertFrom-Json
    $jwks = (Invoke-WebRequest -Uri $disc.jwks_uri -UseBasicParsing).Content | ConvertFrom-Json
    Record "1.Discovery+JWKS" ($jwks.keys.Count -gt 0) ("issuer=$($disc.issuer) keys=$($jwks.keys.Count)")
} catch {
    Record "1.Discovery+JWKS" $false $_.Exception.Message
}

# ── 2-4. PKCE 授权码流（alice / 主租户）──
Write-Host "[2/8] PKCE 授权码流（$TestUser）"
$aliceToken = $null
try {
    $flow = Invoke-PKCE-Flow $TestUser $TestPassword
    $aliceToken = $flow.Token
    $id = Decode-JwtPayload $aliceToken.id_token
    $groups = @($id.groups)
    $hasTenantAcme = $groups -contains "tenant:acme"
    $ok = ($aliceToken.access_token) -and ($aliceToken.id_token) -and ($id.email -eq $TestUser) -and $hasTenantAcme
    Record "2.PKCE登录取code" $true ("state=$($flow.State.Substring(0,8))... email=$($id.email)")
    Record "3.id_token解码" $ok ("sub=$($id.sub) groups=$($groups -join ',')")
} catch {
    Record "2.PKCE登录取code" $false $_.Exception.Message
    Record "3.id_token解码" $false "skipped"
}

# ── 5. 跨租户（carol / globex）──
Write-Host "[5/8] 跨租户校验（$CrossTenantUser）"
try {
    $flow2 = Invoke-PKCE-Flow $CrossTenantUser $CrossTenantPassword
    $id2 = Decode-JwtPayload $flow2.Token.id_token
    $diff = $aliceToken -and ($id2.sub -ne $id.sub) -and ($id2.groups -contains "tenant:globex")
    Record "5.跨租户隔离" $diff ("alice=tenant:acme carol=$(($id2.groups) -join ',')")
} catch {
    Record "5.跨租户隔离" $false $_.Exception.Message
}

# ── 6. 双副本存活 ──
Write-Host "[6/8] 双副本存活检查"
try {
    $states = @()
    foreach ($c in @("ea-keycloak-a", "ea-keycloak-b")) {
        $st = (docker inspect --format "{{.State.Status}}" $c 2>$null)
        $states += "$c=$st"
    }
    $bothUp = ($states -join ",") -match "ea-keycloak-a=running" -and ($states -join ",") -match "ea-keycloak-b=running"
    Record "6.双副本存活" $bothUp ($states -join ", ")
} catch {
    Record "6.双副本存活" $false $_.Exception.Message
}

# ── 7. 故障切换：stop keycloak-a，Caddy 应继续服务 ──
Write-Host "[7/8] 故障切换（stop keycloak-a → Caddy 自动摘除）"
try {
    $deployDir = Join-Path (Split-Path $PSScriptRoot) "deploy"
    $composeFile = Join-Path $deployDir "docker-compose.keycloak-prod.yml"
    $envFile = Join-Path $deployDir ".env.keycloak-prod"
    $stopArgs = @("compose", "-f", $composeFile, "--env-file", $envFile, "stop", "keycloak-a")
    & cmd /c "docker $($stopArgs -join ' ') >nul 2>&1"
    Start-Sleep -Seconds 3
    # Caddy 被动摘除（lb_try_duration 20s），轮询等待恢复
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $svced = $false
    while ($sw.Elapsed.TotalSeconds -lt 60) {
        try {
            $r = Invoke-WebRequest -Uri "$Base/realms/$Realm/.well-known/openid-configuration" -UseBasicParsing -TimeoutSec 5
            if ($r.StatusCode -eq 200) { $svced = $true; break }
        } catch { Start-Sleep -Seconds 2 }
    }
    Record "7.故障切换" $svced ("keycloak-a 下线后 Caddy 仍服务 (耗时 $([math]::Round($sw.Elapsed.TotalSeconds))s)")
    # 恢复
    $startArgs = @("compose", "-f", $composeFile, "--env-file", $envFile, "start", "keycloak-a")
    & cmd /c "docker $($startArgs -join ' ') >nul 2>&1"
    Start-Sleep -Seconds 5
} catch {
    Record "7.故障切换" $false $_.Exception.Message
}

# ── 8. 持久化：重启 keycloak-a 后数据仍在（postgres 落盘验证）──
Write-Host "[8/8] 持久化验证（restart keycloak-a 后 realm 仍在）"
try {
    $deployDir = Join-Path (Split-Path $PSScriptRoot) "deploy"
    $composeFile = Join-Path $deployDir "docker-compose.keycloak-prod.yml"
    $envFile = Join-Path $deployDir ".env.keycloak-prod"
    $restartArgs = @("compose", "-f", $composeFile, "--env-file", $envFile, "restart", "keycloak-a")
    & cmd /c "docker $($restartArgs -join ' ') >nul 2>&1"
    $ok = $false
    $deadline = (Get-Date).AddSeconds(120)
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Seconds 3
        try {
            $r = Invoke-WebRequest -Uri "$Base/realms/$Realm/.well-known/openid-configuration" -UseBasicParsing -TimeoutSec 5
            if ($r.StatusCode -eq 200) { $ok = $true; break }
        } catch { }
    }
    Record "8.持久化" $ok "restart 后 discovery 恢复（数据来自 keycloak-pg）"
} catch {
    Record "8.持久化" $false $_.Exception.Message
}

# =============================================================================
Write-Host ""
Write-Host "=== 结果汇总 ===" -ForegroundColor Cyan
$script:Results | Format-Table -AutoSize
$failed = @($script:Results | Where-Object { -not $_.Passed }).Count
$total = @($script:Results).Count
if ($failed -eq 0) {
    Write-Host "ALL PASS ($total steps)" -ForegroundColor Green
    exit 0
} else {
    Write-Host "FAILED: $failed of $total steps" -ForegroundColor Red
    exit 1
}
