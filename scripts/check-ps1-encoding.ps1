#Requires -Version 5.1
<#
.SYNOPSIS
    Guard: every .ps1 containing non-ASCII bytes MUST carry a UTF-8 BOM.

.DESCRIPTION
    Why this exists
    ---------------
    Windows PowerShell 5.1 reads a script file with no BOM using the system
    ANSI code page (GBK / CP936 on zh-CN Windows), NOT UTF-8. A UTF-8 encoded
    Chinese comment therefore decodes into mojibake, and once a multi-byte
    sequence mis-aligns, it swallows the *following* ASCII quote/brace into a
    double-byte character. The file then fails to parse at all, with confusing
    errors such as (messages are localized; these are the zh-CN renderings):

        ParserError: <string is missing the terminator: '>
        ParserError: <unexpected token '}' in expression or statement>

    The reported line number is also shifted, because the mis-decoding merges
    some line breaks. Result: a script that looks perfectly fine in VS Code is
    completely unrunnable from the command line.

    This actually bit the project - deploy.ps1, build.ps1, sync-web.ps1,
    drill-chaos.ps1, verify-citus-sharding.ps1, verify-keycloak-oidc.ps1,
    setup-integration.ps1 and packaging/install-windows.ps1 were all silently
    broken. Files that happened to have a BOM (start-local.ps1, ci.ps1,
    verify-keycloak-prod.ps1) worked, which made the breakage look random.

    The fix is a 3-byte prefix (EF BB BF). It changes nothing else and is
    accepted by PowerShell 7 as well.

.PARAMETER Fix
    Rewrite offending files in place, prepending the UTF-8 BOM. Content bytes
    after the BOM are left untouched.

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File .\scripts\check-ps1-encoding.ps1
    powershell -ExecutionPolicy Bypass -File .\scripts\check-ps1-encoding.ps1 -Fix
#>
[CmdletBinding()]
param(
    [switch]$Fix
)

$ErrorActionPreference = 'Stop'

$ROOT = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)

$exclude = '\\node_modules\\|\\\.git\\|\\\.ci-logs\\|\\dist\\|\\webroot\\|\\\.workbuddy\\'

$files = @(Get-ChildItem -LiteralPath $ROOT -Recurse -Filter '*.ps1' -File -ErrorAction SilentlyContinue |
    Where-Object { $_.FullName -notmatch $exclude })

$bad = New-Object System.Collections.Generic.List[string]
$nonUtf8 = New-Object System.Collections.Generic.List[string]
$strictUtf8 = New-Object System.Text.UTF8Encoding($false, $true)
$bom = [byte[]](0xEF, 0xBB, 0xBF)

foreach ($f in $files) {
    $bytes = [System.IO.File]::ReadAllBytes($f.FullName)
    if ($bytes.Length -eq 0) { continue }

    $hasBom = ($bytes.Length -ge 3 -and $bytes[0] -eq 0xEF -and $bytes[1] -eq 0xBB -and $bytes[2] -eq 0xBF)
    $start = 0
    if ($hasBom) { $start = 3 }

    # Is the payload valid UTF-8? If not, the file is ANSI-encoded and needs a
    # human decision - silently prepending a BOM would corrupt it further.
    $validUtf8 = $true
    try { [void]$strictUtf8.GetString($bytes, $start, $bytes.Length - $start) }
    catch { $validUtf8 = $false }

    if (-not $validUtf8) {
        $nonUtf8.Add($f.FullName.Replace($ROOT + '\', ''))
        continue
    }

    $hasNonAscii = $false
    for ($i = $start; $i -lt $bytes.Length; $i++) {
        if ($bytes[$i] -gt 127) { $hasNonAscii = $true; break }
    }

    if ($hasNonAscii -and -not $hasBom) {
        $rel = $f.FullName.Replace($ROOT + '\', '')
        if ($Fix) {
            $new = New-Object byte[] ($bom.Length + $bytes.Length)
            [Array]::Copy($bom, 0, $new, 0, $bom.Length)
            [Array]::Copy($bytes, 0, $new, $bom.Length, $bytes.Length)
            [System.IO.File]::WriteAllBytes($f.FullName, $new)
            Write-Host ("FIXED  " + $rel)
        }
        else {
            $bad.Add($rel)
        }
    }
}

$fail = $false

if ($nonUtf8.Count -gt 0) {
    Write-Host "x Not valid UTF-8 (needs manual review, will NOT be auto-fixed):" -ForegroundColor Red
    foreach ($n in $nonUtf8) { Write-Host ("    " + $n) -ForegroundColor Red }
    $fail = $true
}

if ($bad.Count -gt 0) {
    Write-Host ("x " + $bad.Count + " .ps1 file(s) contain non-ASCII text but have no UTF-8 BOM.") -ForegroundColor Red
    Write-Host "  PowerShell 5.1 will decode them as ANSI and fail to parse them." -ForegroundColor Red
    foreach ($n in $bad) { Write-Host ("    " + $n) -ForegroundColor Red }
    Write-Host ""
    Write-Host "  Re-run with -Fix to prepend the BOM." -ForegroundColor Yellow
    $fail = $true
}

if ($fail) { exit 1 }

Write-Host ("OK  " + $files.Count + " .ps1 file(s) checked - all ASCII-only, or UTF-8 with BOM.") -ForegroundColor Green
exit 0
