package tasks

import (
	"context"
	"encoding/json"
	"fmt"

	"ovc-agent/internal/hyperv"
	"ovc-agent/internal/protocol"
)

// HostMetricsResult is one quick-metrics sample for the host. It maps onto the
// backend's <hostid>.host_metrics payload (app/services/inventory.py::
// apply_host_metrics). Intentionally lightweight - this is not a monitoring stack.
type HostMetricsResult struct {
	CPUPercent    *float64         `json:"cpuPercent"`
	MemPercent    *float64         `json:"memPercent"`
	MemUsedBytes  *int64           `json:"memUsedBytes"`
	MemTotalBytes *int64           `json:"memTotalBytes"`
	DiskLatencyMs *float64         `json:"diskLatencyMs"` // avg of read+write on _Total, ms
	NetRxBps      *int64           `json:"netRxBps"`
	NetTxBps      *int64           `json:"netTxBps"`
	Disks         []HostDiskMetric `json:"disks"`
	Net           []HostNetMetric  `json:"net"`
}

type HostDiskMetric struct {
	Name           string  `json:"name"`
	ReadLatencyMs  float64 `json:"readLatencyMs"`
	WriteLatencyMs float64 `json:"writeLatencyMs"`
	QueueLength    float64 `json:"queueLength"`
}

type HostNetMetric struct {
	Name  string `json:"name"`
	RxBps int64  `json:"rxBps"`
	TxBps int64  `json:"txBps"`
}

// toPayload wraps the sample with reported_at for the state queue.
func (r *HostMetricsResult) toPayload(now string) protocol.HostMetricsPayload {
	return protocol.HostMetricsPayload{
		ReportedAt:    now,
		CPUPercent:    r.CPUPercent,
		MemPercent:    r.MemPercent,
		MemUsedBytes:  r.MemUsedBytes,
		MemTotalBytes: r.MemTotalBytes,
		DiskLatencyMs: r.DiskLatencyMs,
		NetRxBps:      r.NetRxBps,
		NetTxBps:      r.NetTxBps,
		Disks:         r.Disks,
		Net:           r.Net,
	}
}

// hostMetricsScript pulls CPU / memory / disk latency / network throughput from
// the non-localized Win32_PerfFormattedData_* CIM classes (counter *paths* are
// localized on non-English Windows; these class + property names are stable).
const hostMetricsScript = `
$out = [ordered]@{}

try {
    $cpu = Get-CimInstance Win32_PerfFormattedData_PerfOS_Processor -Filter "Name='_Total'"
    if ($cpu) { $out.cpuPercent = [double]$cpu.PercentProcessorTime }
} catch {}

try {
    $os = Get-CimInstance Win32_OperatingSystem
    $totalKB = [double]$os.TotalVisibleMemorySize
    $freeKB  = [double]$os.FreePhysicalMemory
    if ($totalKB -gt 0) {
        $out.memPercent    = [math]::Round((($totalKB - $freeKB) / $totalKB) * 100, 2)
        $out.memUsedBytes  = [int64](($totalKB - $freeKB) * 1024)
        $out.memTotalBytes = [int64]($totalKB * 1024)
    }
} catch {}

$disks = @()
try {
    $pd = Get-CimInstance Win32_PerfFormattedData_PerfDisk_PhysicalDisk
    foreach ($d in $pd) {
        if ($d.Name -eq '_Total') {
            $out.diskLatencyMs = [math]::Round(((([double]$d.AvgDisksecPerRead) + ([double]$d.AvgDisksecPerWrite)) / 2) * 1000, 3)
            continue
        }
        $disks += [PSCustomObject]@{
            name           = $d.Name
            readLatencyMs  = [math]::Round(([double]$d.AvgDisksecPerRead) * 1000, 3)
            writeLatencyMs = [math]::Round(([double]$d.AvgDisksecPerWrite) * 1000, 3)
            queueLength    = [double]$d.CurrentDiskQueueLength
        }
    }
} catch {}
$out.disks = $disks

$net = @()
$rxTotal = [int64]0
$txTotal = [int64]0
try {
    $ni = Get-CimInstance Win32_PerfFormattedData_Tcpip_NetworkInterface
    foreach ($n in $ni) {
        $rx = [int64]$n.BytesReceivedPersec
        $tx = [int64]$n.BytesSentPersec
        $rxTotal += $rx
        $txTotal += $tx
        $net += [PSCustomObject]@{ name = $n.Name; rxBps = $rx; txBps = $tx }
    }
    $out.netRxBps = $rxTotal
    $out.netTxBps = $txTotal
} catch {}
$out.net = $net

[PSCustomObject]$out | ConvertTo-Json -Depth 4 -Compress
`

func (d *Dispatcher) handleHostMetrics(ctx context.Context) (*HostMetricsResult, error) {
	output, err := hyperv.RunPowerShell(ctx, hostMetricsScript)
	if err != nil {
		return nil, fmt.Errorf("failed to collect host metrics: %v", err)
	}
	output = trimPowerShellOutput(output)
	if len(output) == 0 {
		return nil, fmt.Errorf("host metrics returned empty output")
	}

	var raw struct {
		HostMetricsResult
		DisksRaw json.RawMessage `json:"disks"`
		NetRaw   json.RawMessage `json:"net"`
	}
	if err := json.Unmarshal(output, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse host metrics JSON: %v (raw: %s)", err, string(output))
	}

	result := raw.HostMetricsResult
	result.Disks = unmarshalList[HostDiskMetric](raw.DisksRaw)
	result.Net = unmarshalList[HostNetMetric](raw.NetRaw)
	return &result, nil
}

// unmarshalList tolerates PowerShell's "single object instead of array" quirk and
// a literal null, always returning a non-nil slice.
func unmarshalList[T any](raw json.RawMessage) []T {
	out := []T{}
	if len(raw) == 0 || string(raw) == "null" {
		return out
	}
	if err := json.Unmarshal(raw, &out); err == nil {
		return out
	}
	var single T
	if err := json.Unmarshal(raw, &single); err == nil {
		return []T{single}
	}
	return []T{}
}
