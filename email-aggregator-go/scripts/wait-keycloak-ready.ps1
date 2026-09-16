# Poll Keycloak production readiness until ready or timeout
param(
    [int]$MaxPolls = 36,
    [int]$IntervalSec = 10
)
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
Add-Type -TypeDefinition "using System.Net; using System.Security.Cryptography.X509Certificates; public class TrustAllCertsPolicy : ICertificatePolicy { public bool CheckValidationResult(ServicePoint sp, X509Certificate cert, WebRequest req, int problem) { return true; } }"
[System.Net.ServicePointManager]::CertificatePolicy = New-Object TrustAllCertsPolicy
for ($i = 0; $i -lt $MaxPolls; $i++) {
    try {
        $r = Invoke-WebRequest -Uri 'https://localhost:8443/realms/email-aggregator/.well-known/openid-configuration' -UseBasicParsing -TimeoutSec 5
        Write-Output ('READY_AT_POLL=' + $i + ' STATUS=' + $r.StatusCode)
        exit 0
    } catch {
        Write-Output ('poll ' + $i + ' not ready yet')
    }
    Start-Sleep -Seconds $IntervalSec
}
Write-Output 'READY_TIMEOUT'
exit 1
