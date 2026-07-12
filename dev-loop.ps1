# dev-loop.ps1 — локальный цикл разработки Windows-хоста.
#
# Каждый круг: убить старые katana/ffmpeg → git pull → собрать нативно (winnative/cgo)
# → запустить. Когда katana завершается (Q в TUI или любой выход), круг повторяется —
# подтягивается свежий код и пересобирается. Остановить весь цикл — закрыть окно
# PowerShell (или Ctrl+C пару раз).
#
# Запуск из корня репозитория:
#   powershell -ExecutionPolicy Bypass -File .\dev-loop.ps1
# Своя сессия (по умолчанию — рабочая):
#   powershell -ExecutionPolicy Bypass -File .\dev-loop.ps1 -Session <uuid>
#
# Разовая установка тулчейна (если ещё нет):
#   winget install --id GoLang.Go -e
#   winget install --id BrechtSanders.WinLibs.POSIX.UCRT -e
# После установки — перезапустить PowerShell, чтобы PATH подхватился.

param(
    [string]$Session = "fde1ff5b-baa5-4d25-b480-546676d1caf0"
)

$repo  = $PSScriptRoot
$proto = Join-Path $repo "proto"
$exe   = Join-Path $env:USERPROFILE ".katana\katana-dev.exe"
$env:CGO_ENABLED = "1"

# Проверка тулчейна — без go/gcc/git нативная сборка невозможна.
foreach ($t in @("go", "gcc", "git")) {
    if (-not (Get-Command $t -ErrorAction SilentlyContinue)) {
        Write-Host "НЕТ '$t' в PATH." -ForegroundColor Red
        Write-Host "Поставь: winget install --id GoLang.Go -e ; winget install --id BrechtSanders.WinLibs.POSIX.UCRT -e" -ForegroundColor Yellow
        Write-Host "Потом перезапусти PowerShell." -ForegroundColor Yellow
        Read-Host "Enter для выхода"
        exit 1
    }
}

Write-Host "dev-loop: репозиторий $repo, сессия $Session" -ForegroundColor DarkGray

while ($true) {
    Write-Host "`n=== [1/3] глушу старые katana / ffmpeg ===" -ForegroundColor Cyan
    Get-Process katana-dev, katana -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    cmd /c "taskkill /F /IM ffmpeg.exe >nul 2>&1"

    Write-Host "=== [2/3] git pull + сборка (winnative, cgo) ===" -ForegroundColor Cyan
    git -C $repo pull --no-rebase
    Push-Location $proto
    go build -tags winnative -o $exe .
    $built = ($LASTEXITCODE -eq 0)
    Pop-Location
    if (-not $built) {
        Write-Host "!!! СБОРКА УПАЛА — жду 4с и пробую снова (поправь код/подожди фикс) !!!" -ForegroundColor Red
        Start-Sleep -Seconds 4
        continue
    }

    Write-Host "=== [3/3] запуск. Q в katana = пересборка+рестарт. Закрой окно = стоп ===" -ForegroundColor Green
    & $exe --session=$Session

    Write-Host "=== katana вышел — перезапуск через 1с... ===" -ForegroundColor Yellow
    Start-Sleep -Seconds 1
}
