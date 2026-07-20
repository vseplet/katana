# dev-loop.ps1 - local Windows host dev loop.
#
# Each cycle: kill old katana/ffmpeg -> git pull -> build native (winnative/cgo) -> run.
# When katana exits (Q in TUI or any exit), the cycle repeats: latest code is pulled
# and rebuilt. To stop the whole loop, close the PowerShell window (or Ctrl+C twice).
#
# Run from repo root:
#   powershell -ExecutionPolicy Bypass -File .\dev-loop.ps1
# Custom session (default is the working one):
#   powershell -ExecutionPolicy Bypass -File .\dev-loop.ps1 -Session <uuid>
#
# One-time toolchain install (if missing):
#   winget install --id GoLang.Go -e
#   winget install --id BrechtSanders.WinLibs.POSIX.UCRT -e
# Restart PowerShell after install so PATH picks up go and gcc.

param(
    [string]$Session = "fde1ff5b-baa5-4d25-b480-546676d1caf0"
)

$repo  = $PSScriptRoot
$proto = Join-Path $repo "proto"
$exe   = Join-Path $env:USERPROFILE ".katana\katana-dev.exe"
# Go runtime panics/SIGSEGV go to stderr (NOT the session .log, which only gets
# log.Printf). Capture stderr to crash.log so a crash header survives for diagnosis.
$crash = Join-Path $env:USERPROFILE ".katana\crash.log"
$env:CGO_ENABLED = "1"

# Toolchain check - native cgo build needs go, gcc, git.
foreach ($t in @("go", "gcc", "git")) {
    if (-not (Get-Command $t -ErrorAction SilentlyContinue)) {
        Write-Host "MISSING '$t' in PATH." -ForegroundColor Red
        Write-Host "Install: winget install --id GoLang.Go -e ; winget install --id BrechtSanders.WinLibs.POSIX.UCRT -e" -ForegroundColor Yellow
        Write-Host "Then restart PowerShell." -ForegroundColor Yellow
        Read-Host "Press Enter to exit"
        exit 1
    }
}

Write-Host "dev-loop: repo $repo, session $Session" -ForegroundColor DarkGray

while ($true) {
    Write-Host "`n=== [1/3] killing old katana / ffmpeg ===" -ForegroundColor Cyan
    Get-Process katana-dev, katana -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    cmd /c "taskkill /F /IM ffmpeg.exe >nul 2>&1"

    Write-Host "=== [2/3] git pull + build (winnative, cgo) ===" -ForegroundColor Cyan
    git -C $repo pull --no-rebase
    Push-Location $proto
    go build -tags winnative -o $exe .
    $built = ($LASTEXITCODE -eq 0)
    Pop-Location
    if (-not $built) {
        Write-Host "!!! BUILD FAILED - waiting 4s, then retry (fix code / wait for push) !!!" -ForegroundColor Red
        Start-Sleep -Seconds 4
        continue
    }

    Write-Host "=== [3/3] running. Q in katana = rebuild+restart. Close window = stop ===" -ForegroundColor Green
    "===== run $(Get-Date -Format o) =====" | Out-File -Append -Encoding utf8 $crash
    # stderr (2) -> crash.log, stdout (TUI) stays on console. Full panic header lands in the file.
    & $exe --session=$Session 2>> $crash

    Write-Host "=== katana exited - restarting in 1s... ===" -ForegroundColor Yellow
    Start-Sleep -Seconds 1
}
