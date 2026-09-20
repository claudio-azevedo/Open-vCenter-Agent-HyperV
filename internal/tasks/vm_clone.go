package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ovc-agent/internal/hyperv"
)

// VMClonePayload contains the parameters for cloning a VM
type VMClonePayload struct {
	// Clone type: "imported" (clone from a running/off VM) or "exported" (clone from exported VM on disk)
	CloneType string `json:"clone_type"`

	// Source VM name (required for "imported" type)
	SourceVMName string `json:"source_vm_name"`

	// Source export path (required for "exported" type - path to the exported VM folder containing .vmcx)
	SourceExportPath string `json:"source_export_path"`

	// Export folder root (required for "imported" - temporary location for export)
	ExportFolderRoot string `json:"export_folder_root"`

	// Destination VM name
	Name string `json:"name"`

	// Destination path for the cloned VM. When empty the agent resolves it from
	// DestinationStorage (same layout rules as vm_create - see resolveVMFolder).
	Path string `json:"path"`

	// Target volume / CSV the clone should live on (e.g. "E:\\",
	// "C:\\ClusterStorage\\Volume2"). Empty → the host's default Hyper-V VM path.
	DestinationStorage string `json:"destination_storage"`

	// VM configuration (applied after import)
	CPUCount int    `json:"cpu_count"`
	MemoryMB int    `json:"memory_mb"`
	Notes    string `json:"notes"`
	VlanID   *int   `json:"vlan_id"`

	// Optional settings
	NestedVirtualization *bool `json:"nested_virtualization"`
	HAEnabled            *bool `json:"ha_enabled"`
	StartNow             *bool `json:"start_now"`

	// ExpandDisks converts every Dynamic disk of the deployed VM to Fixed after
	// import (full-size allocation, predictable performance for long-lived VMs).
	// Only honoured for clone_type "exported". Absent (nil) or true → convert;
	// only an explicit false keeps the template's Dynamic disks untouched.
	ExpandDisks *bool `json:"expand_disks"`
}

// vmCloneTimeout calculates a generous timeout for VM clone operations.
// Export + Import + disk copy can take a long time depending on VM size.
func vmCloneTimeout(p VMClonePayload) time.Duration {
	// Base: 10 minutes for import/export operations + disk copy
	// This is generous because large VMs with big disks can take a while
	return 30 * time.Minute
}

func (d *Dispatcher) handleVMClone(ctx context.Context, taskID string, payload json.RawMessage) (*VMCreateResult, error) {
	if payload == nil || string(payload) == "null" || string(payload) == "{}" {
		return nil, fmt.Errorf("payload is required for vm_clone")
	}

	var p VMClonePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, fmt.Errorf("invalid payload: %v", err)
	}

	// Validate common required fields
	if p.CloneType == "" {
		return nil, fmt.Errorf("clone_type is required (imported or exported)")
	}
	if p.CloneType != "imported" && p.CloneType != "exported" {
		return nil, fmt.Errorf("clone_type must be 'imported' or 'exported'")
	}
	if p.Name == "" {
		return nil, fmt.Errorf("name is required for vm_clone")
	}

	// Destination folder: the backend sends only the target volume; the agent
	// owns the per-VM folder layout (same rules as vm_create).
	if p.Path == "" {
		resolved, err := d.resolveVMFolder(ctx, p.Name, p.DestinationStorage, "")
		if err != nil {
			return nil, fmt.Errorf("could not resolve destination folder: %v", err)
		}
		p.Path = resolved
	}

	// Type-specific validations
	if p.CloneType == "imported" {
		if p.SourceVMName == "" {
			return nil, fmt.Errorf("source_vm_name is required for clone_type 'imported'")
		}
		if p.ExportFolderRoot == "" {
			// A scratch folder next to the destination (same volume → the
			// export-then-import stays a local copy). Removed after the import.
			p.ExportFolderRoot = filepath.Join(filepath.Dir(p.Path), "_ovc_clone_export")
		}
	}
	if p.CloneType == "exported" {
		if p.SourceExportPath == "" {
			return nil, fmt.Errorf("source_export_path is required for clone_type 'exported'")
		}
	}

	// --- Disk space pre-check: calculate source disk sizes natively and verify 3x free space ---
	d.reportProgress(ctx, taskID, TaskVMClone, 2, "Checking available disk space")

	var totalSizeBytes int64

	if p.CloneType == "imported" {
		// Get VHD paths from the source VM (lightweight PS call - no Get-VHD)
		pathScript := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$vm = Get-VM -Name "%s" -ErrorAction SilentlyContinue
if (-not $vm) { throw "Source VM '%s' not found" }
@($vm | Get-VMHardDiskDrive | ForEach-Object { $_.Path }) | ConvertTo-Json -Compress
`, p.SourceVMName, p.SourceVMName)

		pathOutput, err := hyperv.RunPowerShell(ctx, pathScript)
		if err != nil {
			return nil, fmt.Errorf("failed to get source VM disk paths: %s", extractPSErrorDetail(err))
		}
		pathOutput = trimPowerShellOutput(pathOutput)

		var vhdPaths []string
		if len(pathOutput) > 0 {
			if pathOutput[0] == '[' {
				json.Unmarshal(pathOutput, &vhdPaths)
			} else {
				var single string
				if err := json.Unmarshal(pathOutput, &single); err == nil {
					vhdPaths = []string{single}
				}
			}
		}

		for _, path := range vhdPaths {
			if info, err := os.Stat(path); err == nil {
				totalSizeBytes += info.Size()
			}
		}
	} else {
		// For exported: scan directory for .vhdx/.vhd files using Go native
		filepath.Walk(p.SourceExportPath, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			if ext == ".vhdx" || ext == ".vhd" {
				totalSizeBytes += info.Size()
			}
			return nil
		})
	}

	totalSourceDiskGB := float64(totalSizeBytes) / (1024 * 1024 * 1024)

	if totalSourceDiskGB > 0 {
		if err := d.checkDiskSpace(ctx, totalSourceDiskGB, p.Path); err != nil {
			return nil, err
		}
	}

	// Use a dedicated long timeout for clone operations
	cloneCtx, cloneCancel := context.WithTimeout(context.Background(), vmCloneTimeout(p))
	defer cloneCancel()

	// --- Pre-checks: VM name and destination path ---
	d.reportProgress(ctx, taskID, TaskVMClone, 5, "Validating pre-conditions")
	preCheckScript := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'

# Check if VM with destination name already exists
$existing = Get-VM -Name "%s" -ErrorAction SilentlyContinue
if ($existing) {
    throw "VM with name '%s' already exists"
}

# Check if the destination path already exists
if (Test-Path "%s") {
    throw "Directory '%s' already exists"
}
`, p.Name, p.Name, p.Path, p.Path)

	if _, err := hyperv.RunPowerShell(cloneCtx, preCheckScript); err != nil {
		return nil, fmt.Errorf("pre-check failed: %s", extractPSErrorDetail(err))
	}

	// Determine source export path
	sourceExportPath := p.SourceExportPath

	if p.CloneType == "imported" {
		// Additional validations for imported type
		importedPreCheck := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'

$vm = Get-VM -Name "%s" -ErrorAction SilentlyContinue
if (-not $vm) {
    throw "Source VM '%s' not found"
}
if ($vm.State -ne 'Off') {
    throw "Source VM '%s' must be powered off for cloning"
}

# Check for snapshots
$snaps = @(Get-VMSnapshot -VMName "%s" -ErrorAction SilentlyContinue)
if ($snaps.Count -gt 0) {
    throw "Source VM '%s' has snapshots. Remove all snapshots before cloning"
}
`, p.SourceVMName, p.SourceVMName, p.SourceVMName, p.SourceVMName, p.SourceVMName)

		if _, err := hyperv.RunPowerShell(cloneCtx, importedPreCheck); err != nil {
			return nil, fmt.Errorf("source VM validation failed: %s", extractPSErrorDetail(err))
		}

		// Export the source VM with progress reporting (same pattern as Move-VMStorage)
		d.reportProgress(ctx, taskID, TaskVMClone, 10, "Exporting source VM (0%)")
		exportScript := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'

# Ensure export folder root exists
if (-not (Test-Path "%s")) {
    New-Item -ItemType Directory -Path "%s" -Force | Out-Null
}

# Export VM as a job and poll progress
$job = Export-VM -Name "%s" -Path "%s" -AsJob
if (-not $job) {
    throw "Export-VM did not return a job"
}

$jobId = $job.Id
$lastPct = -1
while ($true) {
    $job = Get-Job -Id $jobId -ErrorAction Stop
    $pct = $null
    try {
        if ($job.Progress -and $job.Progress.Count -gt 0) {
            $pct = [int]$job.Progress[-1].PercentComplete
        }
    } catch {}

    if ($null -ne $pct -and $pct -ge 0 -and $pct -ne $lastPct) {
        $lastPct = $pct
        [Console]::Out.WriteLine("PROGRESS:$pct")
        [Console]::Out.Flush()
    }

    if ($job.State -ne 'Running' -and $job.State -ne 'NotStarted') {
        break
    }
    Start-Sleep -Seconds 3
}

if ($job.State -ne 'Completed') {
    $errMsg = ""
    try { $errMsg = $job.ChildJobs[0].JobStateInfo.Reason.Message } catch {}
    if (-not $errMsg) {
        try { $errMsg = ($job | Receive-Job 2>&1 | Out-String).Trim() } catch {}
    }
    if (-not $errMsg) { $errMsg = "job ended in state $($job.State)" }
    Remove-Job $job -Force -ErrorAction SilentlyContinue
    throw "Export failed: $errMsg"
}
Remove-Job $job -Force -ErrorAction SilentlyContinue
`, p.ExportFolderRoot, p.ExportFolderRoot, p.SourceVMName, p.ExportFolderRoot)

		lastExportPercent := 10
		_, err := hyperv.RunLongPowerShellStream(cloneCtx, exportScript, func(line string) {
			if !strings.HasPrefix(line, "PROGRESS:") {
				return
			}
			var jobPercent int
			if _, scanErr := fmt.Sscanf(strings.TrimPrefix(line, "PROGRESS:"), "%d", &jobPercent); scanErr != nil {
				return
			}
			// Map export job 0-100% to task progress 10-38% (export is ~30% of the overall clone)
			mapped := 10 + (jobPercent*28)/100
			if mapped > lastExportPercent {
				lastExportPercent = mapped
				d.reportProgress(ctx, taskID, TaskVMClone, mapped,
					fmt.Sprintf("Exporting source VM (%d%%)", jobPercent))
			}
		})
		if err != nil {
			return nil, fmt.Errorf("failed to export source VM '%s': %s", p.SourceVMName, extractPSErrorDetail(err))
		}

		// The export creates a subfolder with the VM name
		sourceExportPath = p.ExportFolderRoot + "\\" + p.SourceVMName
	}

	// --- Import the VM ---
	d.reportProgress(ctx, taskID, TaskVMClone, 40, "Importing VM and copying disks")

	// Determine original VM name for disk suffix extraction
	originalVMName := p.SourceVMName
	if p.CloneType == "exported" {
		// For exported, try to get the original name from the .vmcx import result
		// We pass the source export path's folder name as a hint
		parts := strings.Split(strings.ReplaceAll(sourceExportPath, "/", "\\"), "\\")
		if len(parts) > 0 {
			originalVMName = parts[len(parts)-1]
		}
	}

	importScript := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'

# Create destination folder
New-Item -ItemType Directory -Path "%s" -Force | Out-Null

# Locate the .vmcx configuration file
$vmcxFile = (Get-ChildItem "%s\Virtual Machines" -Filter *.vmcx -Recurse | Select-Object -First 1).FullName
if (-not $vmcxFile) {
    throw ".vmcx configuration file not found in %s"
}

# Import with copy and new ID
$importParams = @{
    Path               = $vmcxFile
    SnapshotFilePath   = Join-Path "%s" "Snapshots"
    VhdDestinationPath = Join-Path "%s" "Virtual Hard Disks"
    VirtualMachinePath = "%s"
}

$importedVM = Import-VM -Copy -GenerateNewID @importParams -ErrorAction Stop

# Rename the VM to the destination name
$importedVM | Rename-VM -NewName "%s" -ErrorAction Stop

# If VM is in Saved state after import, remove saved state
$cloneVM = Get-VM -Name "%s" -ErrorAction Stop
if ($cloneVM.State -eq 'Saved') {
    $cloneVM | Remove-VMSavedState -ErrorAction Stop
}

# Rename disks: try to preserve original suffix, fallback to OS (single disk) or Disk1/Disk2 (multiple)
$originalVMName = "%s"
$newVMName = "%s"
$allDisks = $cloneVM | Get-VMHardDiskDrive | Sort-Object ControllerType, ControllerNumber, ControllerLocation
$counter = 1
$totalDisks = @($allDisks).Count

foreach ($disk in $allDisks) {
    $ext = [System.IO.Path]::GetExtension($disk.Path)
    $baseName = [System.IO.Path]::GetFileNameWithoutExtension($disk.Path)

    # Try to extract suffix by removing original VM name prefix
    $suffix = ""
    if ($originalVMName -and $baseName.StartsWith($originalVMName, [System.StringComparison]::OrdinalIgnoreCase)) {
        $suffix = $baseName.Substring($originalVMName.Length)
        # Remove leading dash/underscore if present
        if ($suffix.StartsWith("-") -or $suffix.StartsWith("_")) {
            $suffix = $suffix.Substring(1)
        }
    }

    # Build new filename
    if ($suffix) {
        $newFileName = "$newVMName-$suffix$ext"
    } elseif ($totalDisks -eq 1) {
        # Single disk - use "VM-OS" naming
        $newFileName = "$newVMName-OS$ext"
    } else {
        # Multiple disks without extractable suffix - use Disk counter
        $newFileName = "$newVMName-Disk$counter$ext"
    }
    $counter++

    if (Test-Path $disk.Path) {
        $renamedFile = Rename-Item -Path $disk.Path -NewName $newFileName -PassThru -ErrorAction Stop
        Set-VMHardDiskDrive -VMHardDiskDrive $disk -Path $renamedFile.FullName -ErrorAction Stop
    }
}

# Output the VM ID
$cloneVM.Id.ToString()
`, p.Path, sourceExportPath, sourceExportPath,
		p.Path, p.Path, p.Path,
		p.Name, p.Name, originalVMName, p.Name)

	output, err := hyperv.RunPowerShell(cloneCtx, importScript)
	if err != nil {
		// Cleanup on failure
		if p.CloneType == "imported" && sourceExportPath != "" {
			cleanupScript := fmt.Sprintf(`if (Test-Path "%s") { Remove-Item -Path "%s" -Recurse -Force -ErrorAction SilentlyContinue }`, sourceExportPath, sourceExportPath)
			hyperv.RunPowerShell(cloneCtx, cleanupScript)
		}
		return nil, fmt.Errorf("failed to import/clone VM: %s", extractPSErrorDetail(err))
	}

	vmID := strings.TrimSpace(string(trimPowerShellOutput(output)))

	// --- Cleanup export folder for "imported" type ---
	if p.CloneType == "imported" && sourceExportPath != "" {
		d.reportProgress(ctx, taskID, TaskVMClone, 80, "Cleaning up temporary export files")
		cleanupScript := fmt.Sprintf(`if (Test-Path "%s") { Remove-Item -Path "%s" -Recurse -Force -ErrorAction SilentlyContinue }`, sourceExportPath, sourceExportPath)
		hyperv.RunPowerShell(cloneCtx, cleanupScript) // best-effort
	}

	// --- Expand the deployed template's disks to Fixed ---
	// On by default: templates ship Dynamic disks, and a deployed VM normally
	// wants full-size Fixed disks. The user opts out with expand_disks:false.
	if p.CloneType == "exported" && (p.ExpandDisks == nil || *p.ExpandDisks) {
		d.reportProgress(ctx, taskID, TaskVMClone, 82, "Expanding disks to fixed")
		d.expandClonedDisksToFixed(cloneCtx, p.Name)
	}

	// --- Configure VM settings ---
	d.reportProgress(ctx, taskID, TaskVMClone, 85, "Configuring VM settings")
	if p.CPUCount > 0 {
		script := fmt.Sprintf(`Set-VMProcessor -VMName "%s" -Count %d -ErrorAction Stop`, p.Name, p.CPUCount)
		if _, err := hyperv.RunPowerShell(cloneCtx, script); err != nil {
			d.logger.Warn("vm_clone: failed to set CPU count", "vm", p.Name, "error", err)
		}
	}

	if p.MemoryMB > 0 {
		memBytes := int64(p.MemoryMB) * 1024 * 1024
		script := fmt.Sprintf(`Set-VMMemory -VMName "%s" -StartupBytes %d -ErrorAction Stop`, p.Name, memBytes)
		if _, err := hyperv.RunPowerShell(cloneCtx, script); err != nil {
			d.logger.Warn("vm_clone: failed to set memory", "vm", p.Name, "error", err)
		}
	}

	if p.Notes != "" {
		script := fmt.Sprintf(`Set-VM -VMName "%s" -Notes "%s" -ErrorAction Stop`, p.Name, escapePS(p.Notes))
		if _, err := hyperv.RunPowerShell(cloneCtx, script); err != nil {
			d.logger.Warn("vm_clone: failed to set notes", "vm", p.Name, "error", err)
		}
	}

	if p.VlanID != nil {
		script := fmt.Sprintf(`Set-VMNetworkAdapterVlan -VMName "%s" -Access -VlanId %d -ErrorAction Stop`, p.Name, *p.VlanID)
		if _, err := hyperv.RunPowerShell(cloneCtx, script); err != nil {
			d.logger.Warn("vm_clone: failed to set VLAN", "vm", p.Name, "error", err)
		}
	}

	if p.NestedVirtualization != nil && *p.NestedVirtualization {
		script := fmt.Sprintf(`Set-VMProcessor -VMName "%s" -ExposeVirtualizationExtensions $true -ErrorAction Stop`, p.Name)
		if _, err := hyperv.RunPowerShell(cloneCtx, script); err != nil {
			d.logger.Warn("vm_clone: failed to enable nested virtualization", "vm", p.Name, "error", err)
		}
	}

	if p.HAEnabled != nil && *p.HAEnabled {
		script := fmt.Sprintf(`
Add-ClusterVirtualMachineRole -VMName "%s"
Set-VM -Name "%s" -AutomaticStartAction StartIfRunning
Set-VM -Name "%s" -AutomaticStopAction ShutDown
`, p.Name, p.Name, p.Name)
		if _, err := hyperv.RunPowerShell(cloneCtx, script); err != nil {
			d.logger.Warn("vm_clone: failed to configure HA", "vm", p.Name, "error", err)
		}
	}

	// --- Start VM if requested ---
	vmState := "Off"
	if p.StartNow != nil && *p.StartNow {
		d.reportProgress(ctx, taskID, TaskVMClone, 95, "Starting VM")
		script := fmt.Sprintf(`Start-VM -Name "%s" -ErrorAction Stop`, p.Name)
		if _, err := hyperv.RunPowerShell(cloneCtx, script); err != nil {
			d.logger.Warn("vm_clone: failed to start VM after clone", "vm", p.Name, "error", err)
		} else {
			vmState = "Running"
		}
	}

	return &VMCreateResult{
		VMName: p.Name,
		VMID:   vmID,
		State:  vmState,
	}, nil
}

// expandClonedDisksToFixed rewrites every Dynamic disk attached to a VM deployed
// from an exported template as a Fixed disk of the same name (Fixed disks give
// predictable performance for long-lived VMs). Already-Fixed disks are left
// untouched, and a disk whose full (Fixed) size would not fit on its volume is
// skipped. Best-effort and non-fatal: a per-disk failure is logged and the
// Dynamic disk is kept - the VM still boots. The VM must be Off (it is, right
// after import and before StartNow).
func (d *Dispatcher) expandClonedDisksToFixed(ctx context.Context, vmName string) {
	script := fmt.Sprintf(`
$vm = Get-VM -Name "%s" -ErrorAction Stop
foreach ($drv in @($vm | Get-VMHardDiskDrive)) {
    $p = $drv.Path
    $tmp = $null
    try {
        $info = Get-VHD -Path $p -ErrorAction Stop
        if ($info.VhdType -eq 'Fixed') { continue }
        $dir = Split-Path -LiteralPath $p -Parent
        $name = Split-Path -LiteralPath $p -Leaf
        # Get-Volume -FilePath resolves the real volume even for CSV mount points
        # (C:\ClusterStorage\VolumeN); fall back to the PSDrive for odd providers.
        $vol = Get-Volume -FilePath $p -ErrorAction SilentlyContinue
        $free = if ($vol) { [int64]$vol.SizeRemaining } else { [int64](Get-PSDrive -Name ((Get-Item -LiteralPath $dir).PSDrive.Name) -ErrorAction Stop).Free }
        if ($info.Size -gt ($free - 2GB)) {
            [Console]::Out.WriteLine("CONVERT_SKIPPED:" + $name + ": needs ~" + [math]::Round($info.Size / 1GB) + " GB free on the target volume")
            continue
        }
        $tmp = Join-Path $dir ('_ovc_conv_' + $name)
        if (Test-Path -LiteralPath $tmp) { Remove-Item -LiteralPath $tmp -Force -ErrorAction Stop }
        Convert-VHD -Path $p -DestinationPath $tmp -VHDType Fixed -ErrorAction Stop
        Remove-Item -LiteralPath $p -Force -ErrorAction Stop
        Rename-Item -LiteralPath $tmp -NewName $name -ErrorAction Stop
        [Console]::Out.WriteLine("CONVERTED:" + $name)
    } catch {
        if ($tmp -and (Test-Path -LiteralPath $tmp)) { Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue }
        [Console]::Out.WriteLine("CONVERT_FAILED:" + $p + ": " + $_.Exception.Message)
    }
}
`, vmName)

	if _, err := hyperv.RunLongPowerShellStream(ctx, script, func(line string) {
		logDiskConversionLine(d.logger, "vm_clone", line)
	}); err != nil {
		d.logger.Warn("vm_clone: dynamic-to-fixed disk expansion pass failed", "vm", vmName, "error", err)
	}
}
