# One-shot probe of the Keycloak production stack through Caddy 8443.
# Usage: powershell -NoProfile -ExecutionPolicy Bypass -File scripts\probe-kc-8443.ps1
param(
    [string]$Base = "https://localhost:8443",
    [string]$Realm = "email-aggregator"
)
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
[Net.ServicePointManager]::ServerCertificateValidationCallback = { $true }

$paths = @(
    "/health/ready",
    "/health/live",
    "/realms/$Realm/.well-known/openid-configuration"
)
foreach ($p in $paths) {
    $url = $Base + $p
    try {
        $r = Invoke-WebRequest -Uri $url -UseBasicParsing -TimeoutSec 10
        $body = $r.Content
        if ($body.Length -gt 400) { $body = $body.Substring(0, 400) + "..." }
        Write-Host ("OK  " + $r.StatusCode + "  " + $url)
        Write-Host ("    " + $body)
    } catch {
        $code = "n/a"
        if ($_.Exception.Response) { $code = [int]$_.Exception.Response.StatusCode }
        Write-Host ("ERR " + $code + "  " + $url + "  " + $_.Exception.Message)
    }
}
