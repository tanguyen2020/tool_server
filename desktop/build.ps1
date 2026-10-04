# Build the production exe: build/bin/ServerDashboard.exe
# Requires: Go 1.22+ and the Wails CLI (go install github.com/wailsapp/wails/v2/cmd/wails@latest)
$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot
go test ./internal/...
wails build -clean -trimpath -ldflags "-s -w" -skipbindings
Get-Item build/bin/ServerDashboard.exe | Select-Object Name, @{n='Size (MB)'; e={[math]::Round($_.Length / 1MB, 1)}}
