//go:build windows

package preflight

import (
	"context"
	"fmt"
	"strings"

	"ovc-agent/internal/hyperv"
)

// EnsureHyperV verifies the Hyper-V role is installed and usable on this host.
//
// Checks, in order:
//  1. Get-WindowsFeature Hyper-V (Windows Server) - must be Installed.
//  2. Fallback for client SKUs / when Get-WindowsFeature is unavailable:
//     Get-WindowsOptionalFeature Microsoft-Hyper-V - must be Enabled.
//  3. The Hyper-V PowerShell module must be present (Get-Command Get-VM).
func EnsureHyperV(ctx context.Context) error {
	script := `
$ErrorActionPreference = 'SilentlyContinue'
$installed = $false
$detail = ''

if (Get-Command Get-WindowsFeature -ErrorAction SilentlyContinue) {
    $f = Get-WindowsFeature -Name Hyper-V -ErrorAction SilentlyContinue
    if ($f) { $installed = [bool]$f.Installed; $detail = "Get-WindowsFeature Hyper-V Installed=$($f.Installed)" }
}

if (-not $installed -and (Get-Command Get-WindowsOptionalFeature -ErrorAction SilentlyContinue)) {
    $o = Get-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V -ErrorAction SilentlyContinue
    if ($o) { $installed = ($o.State -eq 'Enabled'); $detail = "Get-WindowsOptionalFeature Microsoft-Hyper-V State=$($o.State)" }
}

$hasModule = [bool](Get-Command Get-VM -ErrorAction SilentlyContinue)

[pscustomobject]@{ installed = $installed; hasModule = $hasModule; detail = $detail } | ConvertTo-Json -Compress
`

	out, err := hyperv.RunPowerShellRaw(ctx, script)
	if err != nil {
		return fmt.Errorf("could not query Hyper-V role: %w", err)
	}
	out = strings.TrimSpace(out)

	// Lightweight parse - avoid pulling json into a tiny package check.
	installed := strings.Contains(out, `"installed":true`)
	hasModule := strings.Contains(out, `"hasModule":true`)

	if !installed {
		return fmt.Errorf("Hyper-V role is not installed on this host (%s)", out)
	}
	if !hasModule {
		return fmt.Errorf("Hyper-V role reported installed but the Hyper-V PowerShell module (Get-VM) is missing (%s)", out)
	}
	return nil
}
