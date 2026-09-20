package tasks

import (
	"context"
	"encoding/json"
	"fmt"

	"ovc-agent/internal/hostinfo"
	"ovc-agent/internal/hyperv"
)

// diskSpaceCheckResult holds the parsed result from the PowerShell space check
type diskSpaceCheckResult struct {
	VolumeName string  `json:"volumeName"`
	FreeGB     float64 `json:"freeGb"`
}

// requiredSpaceMultiplier is the safety factor: we require 3x the requested disk space to be free
const requiredSpaceMultiplier = 3.0

// checkDiskSpace verifies that the target storage path has at least 3x the total requested
// disk space available. For cluster hosts, it checks the CSV volume that contains the path.
// For standalone hosts, it uses the native Windows API (GetDiskFreeSpaceEx) directly.
//
// totalDiskGB is the sum of all disk sizes being created/added (in GB).
// targetPath is the destination path where disks will be stored.
func (d *Dispatcher) checkDiskSpace(ctx context.Context, totalDiskGB float64, targetPath string) error {
	requiredFreeGB := totalDiskGB * requiredSpaceMultiplier

	isCluster := d.cluster.IsClusterNode

	// Standalone hosts: use native Go syscall (no PowerShell overhead)
	if !isCluster {
		diskInfo, err := hostinfo.GetDiskSpace(targetPath)
		if err != nil {
			return fmt.Errorf("disk space check failed: %v", err)
		}

		if diskInfo.FreeGB < requiredFreeGB {
			// Extract drive letter for the error message
			volumeName := targetPath[:1] + ":"
			if len(targetPath) >= 2 {
				volumeName = targetPath[:2]
			}
			return fmt.Errorf(
				"insufficient disk space on volume '%s': need %.1f GB free (%.1f GB requested × %.0fx safety margin), but only %.1f GB available",
				volumeName,
				requiredFreeGB,
				totalDiskGB,
				requiredSpaceMultiplier,
				diskInfo.FreeGB,
			)
		}

		volumeName := targetPath[:1] + ":"
		if len(targetPath) >= 2 {
			volumeName = targetPath[:2]
		}
		d.logger.Info("disk space check passed",
			"volume", volumeName,
			"freeGB", diskInfo.FreeGB,
			"requiredGB", requiredFreeGB,
			"requestedGB", totalDiskGB,
		)
		return nil
	}

	// Cluster hosts: need PowerShell to query CSV volumes via Get-ClusterSharedVolume
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$targetPath = "%s"
$result = $null

try {
    $csvs = Get-ClusterSharedVolume -ErrorAction Stop
    foreach ($csv in $csvs) {
        $csvPath = $csv.SharedVolumeInfo.FriendlyVolumeName
        if ($targetPath.StartsWith($csvPath, [System.StringComparison]::OrdinalIgnoreCase)) {
            $volInfo = $csv.SharedVolumeInfo.Partition
            $result = [PSCustomObject]@{
                volumeName = $csv.Name
                freeGb     = [math]::Round($volInfo.FreeSpace / 1GB, 2)
            }
            break
        }
    }
    # If no CSV matched by FriendlyVolumeName, try matching by common ClusterStorage path pattern
    if (-not $result) {
        foreach ($csv in $csvs) {
            $csvPath = $csv.SharedVolumeInfo.FriendlyVolumeName
            $mountPoint = "C:\ClusterStorage\" + ($csv.Name -replace 'Cluster Virtual Disk \(', '' -replace '\)', '' -replace ' ', '')
            if ($targetPath.StartsWith($mountPoint, [System.StringComparison]::OrdinalIgnoreCase) -or
                $targetPath.StartsWith($csvPath, [System.StringComparison]::OrdinalIgnoreCase)) {
                $volInfo = $csv.SharedVolumeInfo.Partition
                $result = [PSCustomObject]@{
                    volumeName = $csv.Name
                    freeGb     = [math]::Round($volInfo.FreeSpace / 1GB, 2)
                }
                break
            }
        }
    }
    # Fallback: if still no match, use the CSV with most free space as reference
    if (-not $result -and $csvs.Count -gt 0) {
        $best = $csvs | Sort-Object { $_.SharedVolumeInfo.Partition.FreeSpace } -Descending | Select-Object -First 1
        $volInfo = $best.SharedVolumeInfo.Partition
        $result = [PSCustomObject]@{
            volumeName = $best.Name + " (fallback)"
            freeGb     = [math]::Round($volInfo.FreeSpace / 1GB, 2)
        }
    }
} catch {
    throw "Failed to query CSV volumes: $($_.Exception.Message)"
}

if (-not $result) {
    throw "Could not determine storage volume for path: $targetPath"
}

$result | ConvertTo-Json -Compress
`, targetPath)

	output, err := hyperv.RunPowerShell(ctx, script)
	if err != nil {
		return fmt.Errorf("disk space check failed: %v", err)
	}

	output = trimPowerShellOutput(output)
	if len(output) == 0 {
		return fmt.Errorf("disk space check returned empty output")
	}

	var spaceResult diskSpaceCheckResult
	if err := json.Unmarshal(output, &spaceResult); err != nil {
		return fmt.Errorf("failed to parse disk space check result: %v (raw: %s)", err, string(output))
	}

	if spaceResult.FreeGB < requiredFreeGB {
		return fmt.Errorf(
			"insufficient disk space on volume '%s': need %.1f GB free (%.1f GB requested × %.0fx safety margin), but only %.1f GB available",
			spaceResult.VolumeName,
			requiredFreeGB,
			totalDiskGB,
			requiredSpaceMultiplier,
			spaceResult.FreeGB,
		)
	}

	d.logger.Info("disk space check passed",
		"volume", spaceResult.VolumeName,
		"freeGB", spaceResult.FreeGB,
		"requiredGB", requiredFreeGB,
		"requestedGB", totalDiskGB,
	)

	return nil
}
