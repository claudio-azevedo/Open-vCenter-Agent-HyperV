package tasks

import (
	"context"
	"fmt"

	"ovc-agent/internal/hyperv"
)

// VMMetricsResult is a quick-metrics sample for every VM that has Hyper-V
// resource metering enabled. Maps onto the backend's <hostid>.vm_metrics payload
// (app/services/inventory.py::apply_vm_metrics). VMs without metering are absent.
type VMMetricsResult struct {
	VMs []VMMetric `json:"vms"`
}

type VMMetric struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	CPUMhz     float64  `json:"cpuMhz"`
	CPUPercent *float64 `json:"cpuPercent"` // best-effort: AvgCPU / (host cores * clock)
	MemBytes   int64    `json:"memBytes"`
	DiskBytes  int64    `json:"diskBytes"`
	NetRxBytes int64    `json:"netRxBytes"`
	NetTxBytes int64    `json:"netTxBytes"`
}

// vmMetricsScript runs Measure-VM (no Reset-VMResourceMetering - the sample is the
// cumulative average/total Hyper-V maintains, rolling at ResourceMeteringSaveInterval).
const vmMetricsScript = `
$hostMHz = 0
try {
    $clock = (Get-CimInstance Win32_Processor | Measure-Object -Property MaxClockSpeed -Average).Average
    $cores = (Get-VMHost).LogicalProcessorCount
    $hostMHz = [double]$clock * [double]$cores
} catch {}

$result = @()
foreach ($vm in (Get-VM | Where-Object { $_.ResourceMeteringEnabled })) {
    $m = $vm | Measure-VM -ErrorAction SilentlyContinue
    if (-not $m) { continue }

    $rx = 0.0; $tx = 0.0
    foreach ($r in $m.NetworkMeteredTrafficReport) {
        if ($r.NetworkDirection -eq 'Inbound') { $rx += [double]$r.TotalTraffic }
        else { $tx += [double]$r.TotalTraffic }
    }

    $cpuPct = $null
    if ($hostMHz -gt 0) { $cpuPct = [math]::Round(([double]$m.AvgCPU / $hostMHz) * 100, 2) }

    $result += [PSCustomObject]@{
        id         = $vm.Id.Guid
        name       = $vm.Name
        cpuMhz     = [double]$m.AvgCPU
        cpuPercent = $cpuPct
        memBytes   = [int64]([double]$m.AvgRAM * 1MB)
        diskBytes  = [int64]([double]$m.TotalDisk * 1MB)
        netRxBytes = [int64]($rx * 1MB)
        netTxBytes = [int64]($tx * 1MB)
    }
}

ConvertTo-Json @($result) -Depth 4 -Compress
`

func (d *Dispatcher) handleVMMetrics(ctx context.Context) (*VMMetricsResult, error) {
	output, err := hyperv.RunPowerShell(ctx, vmMetricsScript)
	if err != nil {
		return nil, fmt.Errorf("failed to collect VM metrics: %v", err)
	}
	output = trimPowerShellOutput(output)

	// No metered VMs -> empty / null output is expected, not an error.
	return &VMMetricsResult{VMs: unmarshalList[VMMetric](output)}, nil
}
