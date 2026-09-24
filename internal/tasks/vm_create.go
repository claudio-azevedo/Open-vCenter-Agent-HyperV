package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"ovc-agent/internal/hyperv"
)

// csvRootRe matches the root of a Cluster Shared Volume path, e.g.
// "C:\ClusterStorage\Volume2" out of "C:\ClusterStorage\Volume2\anything".
var csvRootRe = regexp.MustCompile(`(?i)^([a-zA-Z]:\\ClusterStorage\\[^\\]+)`)

// resolveVMFolder decides the VM's own folder ("<base>\<name>") from the target
// volume the caller picked. The agent owns this layout so a VM is never created
// at the root of the configured VM path:
//
//   - empty / same volume as the host default VM path  → <defaultVMPath>\<name>
//   - a Cluster Shared Volume                          → <CSV root>\VMS\<name>
//   - another local volume (configured aditional_vm_storage) → <root>\VMS\<name>
//
// legacyPath is an older backend's fully-qualified per-VM folder; honored only
// when destinationStorage is empty.
func (d *Dispatcher) resolveVMFolder(ctx context.Context, vmName, destinationStorage, legacyPath string) (string, error) {
	dest := strings.TrimSpace(destinationStorage)
	defaultPath := strings.TrimRight(strings.TrimSpace(d.getDefaultVMPath(ctx)), `\/`)

	if dest == "" {
		if lp := strings.TrimRight(strings.TrimSpace(legacyPath), `\/`); lp != "" {
			return lp, nil
		}
		if defaultPath == "" {
			return "", fmt.Errorf("host default VM path is unavailable; pass destination_storage")
		}
		return filepath.Join(defaultPath, vmName), nil
	}

	destClean := filepath.Clean(dest)

	// Cluster Shared Volume → <CSV root>\VMS\<name>. Checked before the volume
	// match below: a CSV path's drive letter (C:) is not the real C: drive.
	if m := csvRootRe.FindStringSubmatch(destClean); m != nil {
		return filepath.Join(m[1], "VMS", vmName), nil
	}

	// Same volume as the host default VM path → use the default path verbatim.
	if defaultPath != "" && strings.EqualFold(
		strings.ToUpper(filepath.VolumeName(destClean)),
		strings.ToUpper(filepath.VolumeName(defaultPath)),
	) {
		return filepath.Join(defaultPath, vmName), nil
	}

	// Standalone host: the destination must be a configured aditional_vm_storage
	// root (validated the same way vm_move validates its destination).
	if !d.cluster.IsClusterNode {
		root, err := d.resolveStandaloneMoveDestination(ctx, dest)
		if err != nil {
			return "", err
		}
		return filepath.Join(root, "VMS", vmName), nil
	}

	// Cluster host, non-CSV destination - fall back to a HyperV\VMS layout on the
	// requested volume.
	vol := filepath.VolumeName(destClean)
	if vol == "" {
		return "", fmt.Errorf("destination_storage %q is not a recognizable volume", destinationStorage)
	}
	return filepath.Join(vol+`\`, "HyperV", "VMS", vmName), nil
}

// VMCreatePayload contains the parameters for creating a new VM
type VMCreatePayload struct {
	Name     string `json:"name"`
	CPUCount int    `json:"cpu_count"`
	// MemoryMB is the startup RAM. With MemoryDynamic the guest balloons between
	// MemoryMinMB and MemoryMaxMB; 0 for either → a sane default is derived.
	MemoryMB      int  `json:"memory_mb"`
	MemoryDynamic bool `json:"memory_dynamic"`
	MemoryMinMB   int  `json:"memory_min_mb"`
	MemoryMaxMB   int  `json:"memory_max_mb"`
	// Target volume / CSV the VM should live on (e.g. "E:\\",
	// "C:\\ClusterStorage\\Volume2"). Empty → the host's default Hyper-V VM path.
	// The agent resolves the actual per-VM folder from this (see resolveVMFolder).
	DestinationStorage string `json:"destination_storage"`
	// Deprecated: a fully-qualified per-VM folder. Still honored when the backend
	// (an older one) sends it and destination_storage is empty.
	Path                 string         `json:"path"`
	Notes                string         `json:"notes"`
	VlanID               *int           `json:"vlan_id"`
	SwitchName           string         `json:"switch_name"`
	Disks                []VMCreateDisk `json:"disks"`
	NestedVirtualization *bool          `json:"nested_virtualization"`
	HAEnabled            *bool          `json:"ha_enabled"`
	DVD                  string         `json:"dvd"`
	StartNow             *bool          `json:"start_now"`
	OS                   string         `json:"os"`
	// "BIOS" -> Generation 1, "UEFI" (default) -> Generation 2
	Firmware string `json:"firmware"`
}

// generationArg maps the neutral firmware choice to a Hyper-V VM generation.
func (p VMCreatePayload) generationArg() int {
	if strings.EqualFold(p.Firmware, "BIOS") {
		return 1
	}
	return 2
}

// memoryConfigScript returns the Set-VMMemory line for the requested memory
// mode. Startup = memory_mb; for dynamic, min/max fall back to the
// Hyper-V-wizard-style defaults (512 MB / 4× startup) and are clamped so
// min <= startup <= max. The fixed branch pins -DynamicMemoryEnabled $false
// explicitly rather than trusting New-VM's default, which can vary by host.
func (p VMCreatePayload) memoryConfigScript(vmName string) string {
	const mib = 1024 * 1024
	startup := int64(p.MemoryMB) * mib

	if !p.MemoryDynamic {
		return fmt.Sprintf(
			"Set-VMMemory -VMName \"%s\" -DynamicMemoryEnabled $false "+
				"-StartupBytes %d -ErrorAction Stop",
			vmName, startup,
		)
	}

	minB := int64(p.MemoryMinMB) * mib
	if minB <= 0 {
		minB = 512 * mib
	}
	if minB > startup {
		minB = startup
	}

	maxB := int64(p.MemoryMaxMB) * mib
	if maxB <= 0 {
		maxB = startup * 4
	}
	if maxB < startup {
		maxB = startup
	}

	return fmt.Sprintf(
		"Set-VMMemory -VMName \"%s\" -DynamicMemoryEnabled $true "+
			"-MinimumBytes %d -StartupBytes %d -MaximumBytes %d -ErrorAction Stop",
		vmName, minB, startup, maxB,
	)
}

// VMCreateDisk contains the disk parameters for a new VM
type VMCreateDisk struct {
	// Short disk label (e.g. "OS", "DATA") - used in the .vhdx filename
	// "<VMName>-<Name>.vhdx". The agent builds the full path under the resolved
	// VM folder; Path is a fallback for an older backend that sends it directly.
	Name   string  `json:"name"`
	Path   string  `json:"path"`
	Type   string  `json:"type"`
	SizeGB float64 `json:"size_gb"`
}

// VMCreateResult holds the result of a VM creation
type VMCreateResult struct {
	VMName string `json:"vmName"`
	VMID   string `json:"vmId"`
	State  string `json:"state"`
}

// diskTimeout calculates a generous timeout for VHD creation based on size and type.
// Fixed disks need to zero-fill, so they take much longer.
func diskTimeout(disk VMCreateDisk) time.Duration {
	if disk.Type == "Fixed" {
		// ~10s per GB for fixed disks (conservative), minimum 120s
		t := time.Duration(disk.SizeGB*10) * time.Second
		if t < 120*time.Second {
			t = 120 * time.Second
		}
		return t
	}
	// Dynamic disks are fast
	return 60 * time.Second
}

// vmCreateTimeout calculates the total timeout for the entire VM creation process.
// Accounts for disk creation time + extra time for VM setup operations.
func vmCreateTimeout(p VMCreatePayload) time.Duration {
	total := 120 * time.Second // base time for VM creation, config, etc.
	for _, disk := range p.Disks {
		total += diskTimeout(disk)
	}
	return total
}

func (d *Dispatcher) handleVMCreate(ctx context.Context, payload json.RawMessage) (*VMCreateResult, error) {
	if payload == nil || string(payload) == "null" || string(payload) == "{}" {
		return nil, fmt.Errorf("payload is required for vm_create")
	}

	var p VMCreatePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, fmt.Errorf("invalid payload: %v", err)
	}

	// Validate required fields
	if p.Name == "" {
		return nil, fmt.Errorf("name is required for vm_create")
	}
	if p.CPUCount <= 0 {
		return nil, fmt.Errorf("cpu_count must be greater than 0")
	}
	if p.MemoryMB <= 0 {
		return nil, fmt.Errorf("memory_mb must be greater than 0")
	}
	if len(p.Disks) == 0 {
		return nil, fmt.Errorf("disks is required for vm_create (at least one disk)")
	}

	// --- Resolve the VM's own folder from the target volume ---
	// destination_storage is just the volume/CSV; the agent owns the folder layout
	// so a VM never lands at the root of the configured VM path.
	vmFolder, err := d.resolveVMFolder(ctx, p.Name, p.DestinationStorage, p.Path)
	if err != nil {
		return nil, err
	}
	p.Path = vmFolder
	// New-VM -Path takes the PARENT directory and creates a "<VMName>" subfolder
	// under it (same as the Hyper-V wizard). Passing vmFolder there would nest a
	// second "<VMName>" ( ...\Win11-VM\Win11-VM\Virtual Machines ), so New-VM gets
	// the parent while every other path (VHDs, snapshots, paging) uses vmFolder.
	vmParent := filepath.Dir(vmFolder)
	vhdDir := filepath.Join(vmFolder, "Virtual Hard Disks")

	// Validate all disks + fill in the .vhdx path the agent owns.
	for i := range p.Disks {
		if p.Disks[i].SizeGB <= 0 {
			return nil, fmt.Errorf("disks[%d].size_gb must be greater than 0", i)
		}
		if p.Disks[i].Path == "" {
			diskLabel := p.Disks[i].Name
			if diskLabel == "" {
				diskLabel = fmt.Sprintf("disk%d", i)
			}
			p.Disks[i].Path = filepath.Join(vhdDir, fmt.Sprintf("%s-%s.vhdx", p.Name, diskLabel))
		}
	}

	// --- Disk space pre-check: ensure at least 3x total disk size is free ---
	var totalDiskGB float64
	for _, disk := range p.Disks {
		totalDiskGB += disk.SizeGB
	}
	if err := d.checkDiskSpace(ctx, totalDiskGB, p.Path); err != nil {
		return nil, err
	}

	// VM creation can take a long time (Fixed disks, cluster operations).
	// Use a dedicated context independent of the task timeout.
	vmCtx, vmCancel := context.WithTimeout(context.Background(), vmCreateTimeout(p))
	defer vmCancel()

	// --- Step 1: Pre-checks and directory creation ---
	preCheckScript := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'

# Check if VM with this name already exists
$existing = Get-VM -Name "%s" -ErrorAction SilentlyContinue
if ($existing) {
    throw "VM with name '%s' already exists"
}

# Check if the base path already exists
if (Test-Path "%s") {
    throw "Directory '%s' already exists"
}

# Create directory structure. "Virtual Machines" is left to New-VM, which owns
# the config folder; pre-creating it would collide with New-VM -Path.
New-Item -ItemType Directory -Path "%s\Snapshots" -Force | Out-Null
New-Item -ItemType Directory -Path "%s\Virtual Hard Disks" -Force | Out-Null
`, p.Name, p.Name, p.Path, p.Path, p.Path, p.Path)

	if _, err := hyperv.RunPowerShell(vmCtx, preCheckScript); err != nil {
		return nil, fmt.Errorf("pre-check failed for VM '%s': %s", p.Name, extractPSErrorDetail(err))
	}

	// --- Step 2: Create each VHD with its own timeout ---
	for i, disk := range p.Disks {
		diskType := "Dynamic"
		if disk.Type == "Fixed" {
			diskType = "Fixed"
		}
		sizeBytes := int64(disk.SizeGB * 1024 * 1024 * 1024)

		vhdScript := fmt.Sprintf(`New-VHD -Path "%s" -SizeBytes %d -%s | Out-Null`,
			disk.Path, sizeBytes, diskType)

		// Use a dedicated timeout for disk creation (Fixed disks can be slow)
		diskCtx, cancel := context.WithTimeout(context.Background(), diskTimeout(disk))
		_, err := hyperv.RunPowerShell(diskCtx, vhdScript)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("failed to create disk[%d] '%s': %v", i, disk.Path, err)
		}
	}

	// --- Step 3: Create VM with the OS disk (first disk) ---
	osDisk := p.Disks[0]
	memoryBytes := int64(p.MemoryMB) * 1024 * 1024

	// Resolve the virtual switch: use the requested one, else the first that exists.
	switchLiteral := `(Get-VMSwitch | Select-Object -First 1).Name`
	if p.SwitchName != "" {
		switchLiteral = fmt.Sprintf(`"%s"`, escapePS(p.SwitchName))
	}

	createVMScript := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'

$switchName = %s
# -Path is the PARENT dir; Hyper-V creates "<parent>\<VMName>\Virtual Machines"
# for the config. Snapshots / paging are pinned to the VM folder further down.
$vm = New-VM -Name "%s" -MemoryStartupBytes %d -Generation %d -Path "%s" -VHDPath "%s"
if ($switchName) {
    Connect-VMNetworkAdapter -VMName "%s" -SwitchName $switchName -ErrorAction SilentlyContinue
}

# Set CPU count
Set-VMProcessor -VM $vm -Count %d

# Memory: pin the mode explicitly (fixed or dynamic) - don't trust New-VM's default
%s

# Set Start-Stop Action
Set-VM -VM $vm -AutomaticStartAction StartIfRunning
Set-VM -VM $vm -AutomaticStopAction ShutDown

# Keep snapshots + the smart paging file alongside the VM, not at the storage root
Set-VM -VM $vm -SnapshotFileLocation "%s\Snapshots" -SmartPagingFilePath "%s"
`, switchLiteral, p.Name, memoryBytes, p.generationArg(), vmParent, osDisk.Path, p.Name, p.CPUCount, p.memoryConfigScript(p.Name), p.Path, p.Path)

	if _, err := hyperv.RunPowerShell(vmCtx, createVMScript); err != nil {
		return nil, fmt.Errorf("failed to create VM '%s': %v", p.Name, err)
	}

	// --- Step 4: Attach additional disks ---
	for _, disk := range p.Disks[1:] {
		attachScript := fmt.Sprintf(`Add-VMHardDiskDrive -VMName "%s" -Path "%s" -ErrorAction Stop`, p.Name, disk.Path)
		if _, err := hyperv.RunPowerShell(vmCtx, attachScript); err != nil {
			return nil, fmt.Errorf("failed to attach disk '%s': %v", disk.Path, err)
		}
	}

	// --- Step 5: Configure VM options ---
	if p.Notes != "" {
		notesScript := fmt.Sprintf(`Set-VM -VMName "%s" -Notes "%s" -ErrorAction Stop`, p.Name, escapePS(p.Notes))
		if _, err := hyperv.RunPowerShell(vmCtx, notesScript); err != nil {
			d.logger.Warn("failed to set VM notes", "vm", p.Name, "error", err)
		}
	}

	if p.VlanID != nil {
		vlanScript := fmt.Sprintf(`Set-VMNetworkAdapterVlan -VMName "%s" -Access -VlanId %d -ErrorAction Stop`, p.Name, *p.VlanID)
		if _, err := hyperv.RunPowerShell(vmCtx, vlanScript); err != nil {
			d.logger.Warn("failed to set VLAN", "vm", p.Name, "error", err)
		}
	}

	if p.NestedVirtualization != nil && *p.NestedVirtualization {
		nestedScript := fmt.Sprintf(`Set-VMProcessor -VMName "%s" -ExposeVirtualizationExtensions $true -ErrorAction Stop`, p.Name)
		if _, err := hyperv.RunPowerShell(vmCtx, nestedScript); err != nil {
			d.logger.Warn("failed to enable nested virtualization", "vm", p.Name, "error", err)
		}
	}

	if p.HAEnabled != nil && *p.HAEnabled {
		haScript := fmt.Sprintf(`
Add-ClusterVirtualMachineRole -VMName "%s"
Set-VM -Name "%s" -AutomaticStartAction StartIfRunning
Set-VM -Name "%s" -AutomaticStopAction ShutDown
`, p.Name, p.Name, p.Name)
		if _, err := hyperv.RunPowerShell(vmCtx, haScript); err != nil {
			d.logger.Warn("failed to configure HA", "vm", p.Name, "error", err)
		}
	}

	// --- Step 5b: Attach DVD/ISO if specified ---
	if p.DVD != "" {
		dvdScript := fmt.Sprintf(`
$dvd = Get-VMDvdDrive -VMName "%s" -ErrorAction SilentlyContinue | Select-Object -First 1
if ($dvd) {
    Set-VMDvdDrive -VMName "%s" -ControllerNumber $dvd.ControllerNumber -ControllerLocation $dvd.ControllerLocation -Path "%s" -ErrorAction Stop
} else {
    Add-VMDvdDrive -VMName "%s" -Path "%s" -ErrorAction Stop
}
`, p.Name, p.Name, p.DVD, p.Name, p.DVD)
		if _, err := hyperv.RunPowerShell(vmCtx, dvdScript); err != nil {
			d.logger.Warn("failed to attach DVD/ISO", "vm", p.Name, "iso", p.DVD, "error", err)
		}
	}

	// --- Steps 5c/5d: Secure Boot + boot order (UEFI/gen 2 only) ---
	// Generation 1 (BIOS) VMs have no Set-VMFirmware; boot order is fixed
	// (DVD → HD) by the BIOS, so nothing to configure.
	if p.generationArg() == 2 {
		// Secure Boot based on OS type
		if strings.EqualFold(p.OS, "linux") {
			sbScript := fmt.Sprintf(`Set-VMFirmware -VMName "%s" -EnableSecureBoot On -SecureBootTemplate "MicrosoftUEFICertificateAuthority" -ErrorAction Stop`, p.Name)
			if _, err := hyperv.RunPowerShell(vmCtx, sbScript); err != nil {
				d.logger.Warn("failed to set Secure Boot template for Linux", "vm", p.Name, "error", err)
			}
		} else if strings.EqualFold(p.OS, "other") {
			sbScript := fmt.Sprintf(`Set-VMFirmware -VMName "%s" -EnableSecureBoot Off -ErrorAction Stop`, p.Name)
			if _, err := hyperv.RunPowerShell(vmCtx, sbScript); err != nil {
				d.logger.Warn("failed to disable Secure Boot", "vm", p.Name, "error", err)
			}
		}

		// Boot order. Only the OS disk (attached by New-VM -VHDPath, so the
		// lowest controller/location) goes in the list; with multiple disks
		// Get-VMHardDiskDrive returns an array, and "$DVD, $HD" would build a
		// nested array that -BootOrder cannot bind.
		var bootScript string
		if p.DVD != "" {
			bootScript = fmt.Sprintf(`
$HD = Get-VMHardDiskDrive -VMName "%s" | Sort-Object ControllerNumber, ControllerLocation | Select-Object -First 1
$DVD = Get-VMDvdDrive -VMName "%s" | Select-Object -First 1
Set-VMFirmware -VMName "%s" -BootOrder @($DVD, $HD) -ErrorAction Stop
`, p.Name, p.Name, p.Name)
		} else {
			bootScript = fmt.Sprintf(`
$HD = Get-VMHardDiskDrive -VMName "%s" | Sort-Object ControllerNumber, ControllerLocation | Select-Object -First 1
Set-VMFirmware -VMName "%s" -BootOrder $HD -ErrorAction Stop
`, p.Name, p.Name)
		}
		if _, err := hyperv.RunPowerShell(vmCtx, bootScript); err != nil {
			d.logger.Warn("failed to set boot order", "vm", p.Name, "error", err)
		}
	}

	// --- Step 6: Get the VM ID ---
	vmID, err := hyperv.RunPowerShellRaw(vmCtx, fmt.Sprintf(`(Get-VM -Name "%s").Id.ToString()`, p.Name))
	if err != nil {
		return nil, fmt.Errorf("VM created but failed to get ID: %v", err)
	}

	// --- Step 7: Start VM if requested ---
	vmState := "Off"
	if p.StartNow != nil && *p.StartNow {
		startScript := fmt.Sprintf(`Start-VM -Name "%s" -ErrorAction Stop`, p.Name)
		if _, err := hyperv.RunPowerShell(vmCtx, startScript); err != nil {
			d.logger.Warn("failed to start VM after creation", "vm", p.Name, "error", err)
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

// escapePS escapes double quotes and newlines for PowerShell strings
func escapePS(s string) string {
	result := ""
	for _, c := range s {
		if c == '"' {
			result += "`\""
		} else if c == '\n' {
			result += "`n"
		} else if c == '\r' {
			result += "`r"
		} else {
			result += string(c)
		}
	}
	return result
}
