# amf-probe.ps1 - standalone AMD AMF SDK probe. Does NOT touch the live katana stream:
# it builds and runs a separate binary (proto/cmd/amfprobe) that checks whether AMF
# works from our mingw build and whether AMD honors slices + intra-refresh (which
# Media Foundation silently ignored: 1.0 slices/frame, ir=IGNORED).
#
# Run from repo root (safe to run while dev-loop / a stream is active):
#   powershell -ExecutionPolicy Bypass -File .\amf-probe.ps1

$repo  = $PSScriptRoot
$proto = Join-Path $repo "proto"
$exe   = Join-Path $env:USERPROFILE ".katana\amfprobe.exe"
$env:CGO_ENABLED = "1"

foreach ($t in @("go", "gcc")) {
    if (-not (Get-Command $t -ErrorAction SilentlyContinue)) {
        Write-Host "MISSING '$t' in PATH." -ForegroundColor Red
        exit 1
    }
}

Write-Host "=== building amfprobe (winnative, cgo) ===" -ForegroundColor Cyan
Push-Location $proto
go build -tags winnative -o $exe ./cmd/amfprobe
$ok = ($LASTEXITCODE -eq 0)
Pop-Location
if (-not $ok) {
    Write-Host "!!! BUILD FAILED !!!" -ForegroundColor Red
    exit 1
}

Write-Host "=== running amfprobe ===" -ForegroundColor Green
& $exe
Write-Host "`n(лог также в $env:USERPROFILE\.katana\amfprobe.log)" -ForegroundColor DarkGray
