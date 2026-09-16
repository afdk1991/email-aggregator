$deploy = $PSScriptRoot
$export = Join-Path $deploy 'keycloak\realm-export.json'
$tokenFile = Join-Path $deploy 'kc-token.json'

curl.exe -sk -X POST 'https://localhost:8443/realms/master/protocol/openid-connect/token' -d 'grant_type=password&client_id=admin-cli&username=admin&password=admin-secret' -o $tokenFile | Out-Null
$tok = (Get-Content $tokenFile -Raw | ConvertFrom-Json).access_token
Write-Host ("TOKEN LEN " + $tok.Length)

$code1 = curl.exe -sk -o NUL -w '%{http_code}' -X DELETE 'https://localhost:8443/admin/realms/zz-test-min' -H "Authorization: Bearer $tok"
Write-Host "DELETE zz-test-min HTTP $code1"

$code2 = curl.exe -sk -o NUL -w '%{http_code}' -X POST 'https://localhost:8443/admin/realms' -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' --data "@$export"
Write-Host "IMPORT realm-export.json HTTP $code2"

$code3 = curl.exe -sk -o NUL -w '%{http_code}' 'https://localhost:8443/realms/email-aggregator/.well-known/openid-configuration'
Write-Host "DISCOVERY email-aggregator HTTP $code3"
