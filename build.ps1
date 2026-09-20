<#
    Build the ovc-agent Windows binary (native, on Windows).

      .\build.ps1                  -> .\ovc-agent.exe  (windows/amd64)
      .\build.ps1 dist\agent.exe   -> custom output path
      .\build.ps1 -Arch arm64      -> override target arch

    Needs Go on PATH (https://go.dev/dl/).
#>
[CmdletBinding()]
param(
    [string]$Out  = 'ovc-agent.exe',
    [string]$Os   = 'windows',
    [string]$Arch = 'amd64'
)

$ErrorActionPreference = 'Stop'
Set-Location -Path $PSScriptRoot

$version = (Select-String -Path 'internal/tasks/agent_status.go' `
    -Pattern 'AgentVersion = "([^"]+)"').Matches[0].Groups[1].Value

Write-Host "Building ovc-agent v$version  ($Os/$Arch)  ->  $Out"

$old = @{ GOOS = $env:GOOS; GOARCH = $env:GOARCH; CGO_ENABLED = $env:CGO_ENABLED }
try {
    $env:GOOS = $Os
    $env:GOARCH = $Arch
    $env:CGO_ENABLED = '0'
    go build -trimpath -ldflags '-s -w' -o $Out ./cmd/agent
    if ($LASTEXITCODE -ne 0) { throw "go build failed ($LASTEXITCODE)" }
}
finally {
    $env:GOOS = $old.GOOS
    $env:GOARCH = $old.GOARCH
    $env:CGO_ENABLED = $old.CGO_ENABLED
}

$item = Get-Item $Out
Write-Host ("Done: {0}  ({1:N1} MB)" -f $item.Name, ($item.Length / 1MB))
