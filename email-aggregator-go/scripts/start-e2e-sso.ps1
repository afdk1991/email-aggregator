# =============================================================================
# Phase 5 / P5-K2: start the E2E SSO Go service (e2e-sso.exe) with local env.
# Pure ASCII on purpose (avoids PS 5.1 BOM pitfalls).
# Host port 18080 (host 8080 is occupied by another local gateway service).
# OIDC_REDIRECT_URI stays the registered value https://localhost:8080/api/auth/callback
# (Keycloak validates it at token exchange); the E2E script drives the callback
# directly against 18080 after extracting code/state from the 302 Location.
# =============================================================================
$ErrorActionPreference = "Stop"

$env:HTTP_PORT         = "18080"
$env:PG_DSN            = "postgres://agg:agg-secret@localhost:15432/mailagg"
$env:OPENSEARCH_ADDR   = "https://localhost:9200"
$env:KAFKA_BROKERS     = "localhost:9092"
$env:MINIO_ENDPOINT    = "localhost:9000"
$env:MINIO_ACCESS_KEY  = "agg"
$env:MINIO_SECRET_KEY  = "agg-secret"
$env:MINIO_SECURE      = "false"
$env:MINIO_BUCKET      = "agg-mail"
$env:OIDC_ISSUER       = "https://localhost:8443/realms/email-aggregator"
$env:OIDC_CLIENT_ID    = "email-aggregator-go"
$env:OIDC_CLIENT_SECRET = "ea-dev-secret-2026"
$env:OIDC_REDIRECT_URI = "https://localhost:8080/api/auth/callback"

Set-Location -Path (Join-Path $PSScriptRoot "..")
Write-Host "[start-e2e-sso] launching e2e-sso.exe on http://localhost:18080"
& .\e2e-sso.exe
