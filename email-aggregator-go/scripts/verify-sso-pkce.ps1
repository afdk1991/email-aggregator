# =============================================================================
# Phase 5 / P5-K2: SSO BFF end-to-end verification (PKCE authorization code flow)
#
# Flow: Go /api/auth/login -> Keycloak form login -> 302 with code+state
#       -> Go /api/auth/callback (token exchange + id_token verification)
#       -> Bearer access_token against /api/mails
#
# Preconditions:
#   - Keycloak prod stack up (docker-compose.keycloak-prod.yml) on 8443
#   - Go server built with -tags integration,sso listening on $AppBase
#     (HTTP_PORT=18080 locally because host 8080 is taken by another service;
#      OIDC_REDIRECT_URI stays the registered https://localhost:8080/...)
#
# Run:
#   powershell -NoProfile -File scripts\verify-sso-pkce.ps1
#
# Exit code: 0 = all pass; 1 = any failure
# Pure ASCII on purpose: Windows PowerShell 5.1 needs UTF-8 BOM for Chinese
# text in .ps1 files; ASCII avoids the trap entirely.
# =============================================================================
param(
    [string]$AppBase = "http://localhost:18080",
    [string]$KcBase = "https://localhost:8443",
    [string]$Realm = "email-aggregator",
    [string]$ClientId = "email-aggregator-go",
    [string]$TestUser = "alice@acme.test",
    [string]$TestPassword = "alice-secret-2026",
    [string]$RegisteredRedirectUri = "https://localhost:8080/api/auth/callback"
)

$ErrorActionPreference = "Stop"

[Net.ServicePointManager]::ServerCertificateValidationCallback = { $true }
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$script:Results = @()
function Record($name, $ok, $detail) {
    $script:Results += [pscustomobject]@{ Step = $name; Passed = [bool]$ok; Detail = $detail }
    if ($ok) { $mark = "PASS" } else { $mark = "FAIL" }
    Write-Host ("  [{0}] {1}  {2}" -f $mark, $name, $detail)
}

function Get-RedirectLocation($uri, $session) {
    # PS 5.1 returns the 302 response with -MaximumRedirection 0; some builds
    # throw instead - handle both by falling back to Exception.Response.
    try {
        $r = Invoke-WebRequest -Uri $uri -WebSession $session -UseBasicParsing -TimeoutSec 20 -MaximumRedirection 0
        return [string]$r.Headers["Location"]
    } catch {
        $resp = $_.Exception.Response
        if ($null -ne $resp -and $null -ne $resp.Headers) { return [string]$resp.Headers["Location"] }
        throw
    }
}

function Get-ExpectingError($uri, $headers) {
    # Returns a [status, body] pair for a request expected to fail (4xx/5xx).
    try {
        $r = Invoke-WebRequest -Uri $uri -UseBasicParsing -TimeoutSec 20 -Headers $headers
        return [pscustomobject]@{ Status = [int]$r.StatusCode; Body = $r.Content }
    } catch {
        $resp = $_.Exception.Response
        if ($null -eq $resp) { throw }
        $reader = New-Object System.IO.StreamReader($resp.GetResponseStream())
        $body = $reader.ReadToEnd()
        return [pscustomobject]@{ Status = [int]$resp.StatusCode; Body = $body }
    }
}

Write-Host ""
Write-Host "=== P5-K2 SSO PKCE E2E ($AppBase -> $KcBase) ===" -ForegroundColor Cyan

# ---- A. login: 302 to IdP with state + PKCE S256 ----
Write-Host "[A] GET /api/auth/login"
$authUrl = ""
try {
    $s = New-Object Microsoft.PowerShell.Commands.WebRequestSession
    $loc = Get-RedirectLocation "$AppBase/api/auth/login" $s
    $authUrl = $loc
    $hasState = $loc -match "[?&]state=([0-9a-f]{32})"
    $stateA = ""
    if ($hasState) { $stateA = $Matches[1] }
    $ok = $hasState -and ($loc -match "code_challenge=[^&]+") -and ($loc -match "code_challenge_method=S256") -and ($loc -like "$KcBase/realms/$Realm/*")
    Record "A.login-302" $ok ("state=$($stateA.Substring(0,8))... method=S256 challenge=yes")
} catch {
    Record "A.login-302" $false $_.Exception.Message
    $stateA = ""
}

# ---- B. Keycloak form flow: submit credentials, capture code+state ----
Write-Host "[B] Keycloak form login ($TestUser)"
$code = ""
try {
    if ([string]::IsNullOrEmpty($authUrl)) { throw "no auth URL from step A" }
    $loginPage = Invoke-WebRequest -Uri $authUrl -WebSession $s -UseBasicParsing -TimeoutSec 20
    $formMatch = [regex]::Match($loginPage.Content, '<form[^>]+action="([^"]+)"')
    if (-not $formMatch.Success) { throw "login form action not found" }
    $action = $formMatch.Groups[1].Value
    if ($action -notlike "http*") { $action = "$KcBase/realms/$Realm$($action -replace '/api/realms/', '/')" }

    $resp = Invoke-WebRequest -Uri $action -Method Post -WebSession $s -UseBasicParsing -TimeoutSec 20 `
        -MaximumRedirection 0 `
        -Body @{ username = $TestUser; password = $TestPassword; tab = "login" }
    $loc2 = [string]$resp.Headers["Location"]
    if ([string]::IsNullOrEmpty($loc2)) { $loc2 = $resp.ResponseUrl.ToString() }
    if ($loc2 -notmatch "[?&]code=([^&]+)") { throw "authorization code not returned: $loc2" }
    $code = [uri]::UnescapeDataString($Matches[1])
    $stateB = ""
    if ($loc2 -match "[?&]state=([^&]+)") { $stateB = [uri]::UnescapeDataString($Matches[1]) }
    $ok = ($stateB -eq $stateA)
    Record "B.form-login" $ok ("code=$($code.Substring(0,8))... state echoed=$stateB")
} catch {
    Record "B.form-login" $false $_.Exception.Message
}

# ---- C. callback: token exchange + id_token verification + session cookie ----
Write-Host "[C] GET /api/auth/callback"
$accessToken = ""
try {
    if ([string]::IsNullOrEmpty($code)) { throw "no code from step B" }
    $cbUrl = "$AppBase/api/auth/callback?code=$([uri]::EscapeDataString($code))&state=$([uri]::EscapeDataString($stateA))"
    $cb = Invoke-WebRequest -Uri $cbUrl -UseBasicParsing -TimeoutSec 30
    $j = $cb.Content | ConvertFrom-Json
    $cookieHdr = [string]($cb.Headers["Set-Cookie"])
    $hasCookie = $cookieHdr -like "*ea_session=*"
    $ok = ($cb.StatusCode -eq 200) -and ($j.ok -eq $true) -and ($j.access_token -ne "") -and ($j.id_token -ne "") -and ($j.email -eq $TestUser)
    $accessToken = [string]$j.access_token
    Record "C.callback" $ok ("status=$($cb.StatusCode) email=$($j.email) tenant=$($j.tenant_id) cookie_ea_session=$hasCookie")
} catch {
    Record "C.callback" $false $_.Exception.Message
}

# ---- D. Bearer access_token against business API ----
Write-Host "[D] Bearer access_token -> GET /api/mails"
try {
    if ([string]::IsNullOrEmpty($accessToken)) { throw "no access_token from step C" }
    $r = Invoke-WebRequest -Uri "$AppBase/api/mails" -Headers @{ Authorization = "Bearer $accessToken" } -UseBasicParsing -TimeoutSec 30
    $j = $r.Content | ConvertFrom-Json
    $ok = ($r.StatusCode -eq 200) -and ($null -ne $j.mails) -and ($j.PSObject.Properties.Name -contains "count")
    Record "D.api-mails" $ok ("status=$($r.StatusCode) tenant=$($j.tenantId) count=$($j.count)")
} catch {
    Record "D.api-mails" $false $_.Exception.Message
}

# ---- E. negative: unknown state must be rejected with 400 ----
Write-Host "[E] negative: unknown state -> 400"
try {
    $badState = -join ((1..32) | ForEach-Object { (Get-Random -Maximum 16).ToString("x") })
    $neg = Get-ExpectingError "$AppBase/api/auth/callback?code=deadbeef&state=$badState" @{}
    $ok = ($neg.Status -eq 400)
    Record "E.negative-state" $ok ("status=$($neg.Status) body=$($neg.Body)")
} catch {
    Record "E.negative-state" $false $_.Exception.Message
}

Write-Host ""
Write-Host "=== Result Summary ===" -ForegroundColor Cyan
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
