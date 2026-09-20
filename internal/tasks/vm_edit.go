package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"ovc-agent/internal/hyperv"
)

// VMEditPayload contains the parameters for editing a VM (all optional except vm_id)
type VMEditPayload struct {
	VMID                 string          `json:"vm_id"`
	CPUCount             *int            `json:"cpu_count"`
	MemoryMB             *int            `json:"memory_mb"`
	MemoryDynamic        *bool           `json:"memory_dynamic"`
	MemoryMinMB          *int            `json:"memory_min_mb"`
	MemoryMaxMB          *int            `json:"memory_max_mb"`
	Notes                *string         `json:"notes"`
	VlanID               *int            `json:"vlan_id"`
	NestedVirtualization *bool           `json:"nested_virtualization"`
	SecureBoot           *bool           `json:"secure_boot"`
	SecureBootTemplate   *string         `json:"secure_boot_template"`
	AutomaticStart       *string         `json:"automatic_start"`
	AutomaticStartDelay  *int            `json:"automatic_start_delay"`
	AutomaticStop        *string         `json:"automatic_stop"`
	AddDisk              json.RawMessage `json:"add_disk"`
	EditDisk             json.RawMessage `json:"edit_disk"`
	RemoveDisk           json.RawMessage `json:"remove_disk"`
	AddNIC               json.RawMessage `json:"add_nic"`
	EditNIC              json.RawMessage `json:"edit_nic"`
	RemoveNIC            json.RawMessage `json:"remove_nic"`
}

// VMEditDisk contains the parameters for adding a disk to a VM
type VMEditDisk struct {
	Path   string  `json:"path"`
	Type   string  `json:"type"`
	SizeGB float64 `json:"size_gb"`
}

// VMEditDiskResize contains the parameters for expanding a disk
type VMEditDiskResize struct {
	Path   string  `json:"path"`
	SizeGB float64 `json:"size_gb"`
}

// VMRemoveDisk contains the parameters for removing a disk from a VM
type VMRemoveDisk struct {
	Path           string `json:"path"`
	DeleteFromDisk bool   `json:"delete_from_disk"`
}

// VMAddNIC contains the parameters for adding a NIC to a VM
type VMAddNIC struct {
	Name   string `json:"name"`
	VlanID *int   `json:"vlan_id"`
	// SwitchName is the virtual switch to connect the new adapter to. Empty /
	// omitted ⇒ the first switch available on this host.
	SwitchName *string `json:"switch_name"`
}

// VMEditNIC contains the parameters for editing a NIC (rename, VLAN and/or
// virtual-switch change)
type VMEditNIC struct {
	NicID  string  `json:"nic_id"`
	Name   *string `json:"name"`
	VlanID *int    `json:"vlan_id"`
	// SwitchName, when set, reconnects the adapter to that virtual switch (which
	// must exist on the host the VM currently runs on).
	SwitchName *string `json:"switch_name"`
}

// switchResolvePS emits PowerShell that sets $switchName to the virtual switch
// to use. When a name is requested it must exist on this host (else a throw that
// lists what is available); otherwise the first switch on the host is used.
func switchResolvePS(requested *string) string {
	if requested != nil && *requested != "" {
		return fmt.Sprintf(`
$want = "%s"
$switchName = (Get-VMSwitch -Name $want -ErrorAction SilentlyContinue | Select-Object -First 1 -ExpandProperty Name)
if (-not $switchName) {
    $avail = (Get-VMSwitch | Select-Object -ExpandProperty Name) -join ", "
    throw "virtual switch '$want' not found on this host (available: $avail)"
}
`, escapePS(*requested))
	}
	return `
$switchName = (Get-VMSwitch | Select-Object -First 1 -ExpandProperty Name)
if (-not $switchName) { throw "no virtual switch available on this host" }
`
}

// VMRemoveNIC contains the parameters for removing a NIC from a VM
type VMRemoveNIC struct {
	NicID string `json:"nic_id"`
}

// VMEditResult holds the result of a VM edit
type VMEditResult struct {
	VMID    string   `json:"vmId"`
	VMName  string   `json:"vmName"`
	Applied []string `json:"applied"`
}

func (d *Dispatcher) handleVMEdit(ctx context.Context, taskID string, payload json.RawMessage) (*VMEditResult, error) {
	if payload == nil || string(payload) == "null" || string(payload) == "{}" {
		return nil, fmt.Errorf("payload is required for vm_edit")
	}

	var p VMEditPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, fmt.Errorf("invalid payload: %v", err)
	}

	if p.VMID == "" {
		return nil, fmt.Errorf("vm_id is required for vm_edit")
	}

	// Only firmware / static-hardware changes need the VM powered off. Disk and
	// NIC operations, notes and the start/stop policy can be applied live.
	needsOff := p.CPUCount != nil ||
		p.MemoryMB != nil ||
		p.MemoryDynamic != nil ||
		p.MemoryMinMB != nil ||
		p.MemoryMaxMB != nil ||
		p.NestedVirtualization != nil ||
		p.SecureBoot != nil ||
		p.SecureBootTemplate != nil

	offCheck := ""
	if needsOff {
		offCheck = `if ($vm.State -ne 'Off') { throw "VM must be Off to change CPU, memory, Secure Boot or nested virtualization (current state: $($vm.State))" }`
	}
	checkScript := fmt.Sprintf(`
$vm = Get-VM | Where-Object { $_.Id.ToString() -eq "%s" }
if (-not $vm) { throw "VM with id '%s' not found" }
%s
$vm.Name
`, p.VMID, p.VMID, offCheck)

	vmName, err := hyperv.RunPowerShellRaw(ctx, checkScript)
	if err != nil {
		return nil, fmt.Errorf("cannot edit VM: %s", extractPSErrorDetail(err))
	}

	applied := []string{}

	// Apply CPU change
	if p.CPUCount != nil && *p.CPUCount > 0 {
		script := fmt.Sprintf(`Set-VMProcessor -VMName "%s" -Count %d -ErrorAction Stop`, vmName, *p.CPUCount)
		if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
			return nil, fmt.Errorf("failed to set CPU count: %s", extractPSErrorDetail(err))
		}
		applied = append(applied, fmt.Sprintf("cpu_count=%d", *p.CPUCount))
	}

	// Apply memory change - startup size, dynamic/static mode and the dynamic
	// min/max floor/ceiling, in a single Set-VMMemory call. Any field left unset
	// keeps the VM's current value (read live in PowerShell); min/max are clamped
	// so min <= startup <= max, matching the Hyper-V wizard.
	if p.MemoryMB != nil || p.MemoryDynamic != nil || p.MemoryMinMB != nil || p.MemoryMaxMB != nil {
		script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$vm = Get-VM -Name "%s"
$startup = %s
$dynamic = %s
if ($dynamic) {
    $min = %s
    $max = %s
    if ($min -gt $startup) { $min = $startup }
    if ($max -lt $startup) { $max = $startup }
    Set-VMMemory -VMName "%s" -DynamicMemoryEnabled $true -MinimumBytes $min -StartupBytes $startup -MaximumBytes $max
} else {
    Set-VMMemory -VMName "%s" -DynamicMemoryEnabled $false -StartupBytes $startup
}
`,
			vmName,
			psInt64OrExpr(mbToBytes(p.MemoryMB), "$vm.MemoryStartup"),
			psBoolOrExpr(p.MemoryDynamic, "$vm.DynamicMemoryEnabled"),
			psInt64OrExpr(mbToBytes(p.MemoryMinMB), "$vm.MemoryMinimum"),
			psInt64OrExpr(mbToBytes(p.MemoryMaxMB), "$vm.MemoryMaximum"),
			vmName, vmName,
		)
		if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
			return nil, fmt.Errorf("failed to set memory: %s", extractPSErrorDetail(err))
		}
		applied = append(applied, "memory")
	}

	// Apply notes change
	if p.Notes != nil {
		script := fmt.Sprintf(`Set-VM -VMName "%s" -Notes "%s" -ErrorAction Stop`, vmName, escapePS(*p.Notes))
		if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
			return nil, fmt.Errorf("failed to set notes: %s", extractPSErrorDetail(err))
		}
		applied = append(applied, "notes")
	}

	// Apply VLAN change
	if p.VlanID != nil {
		script := fmt.Sprintf(`Set-VMNetworkAdapterVlan -VMName "%s" -Access -VlanId %d -ErrorAction Stop`, vmName, *p.VlanID)
		if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
			return nil, fmt.Errorf("failed to set VLAN: %s", extractPSErrorDetail(err))
		}
		applied = append(applied, fmt.Sprintf("vlan_id=%d", *p.VlanID))
	}

	// Apply nested virtualization change
	if p.NestedVirtualization != nil {
		script := fmt.Sprintf(`Set-VMProcessor -VMName "%s" -ExposeVirtualizationExtensions $%t -ErrorAction Stop`, vmName, *p.NestedVirtualization)
		if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
			return nil, fmt.Errorf("failed to set nested virtualization: %s", extractPSErrorDetail(err))
		}
		applied = append(applied, fmt.Sprintf("nested_virtualization=%t", *p.NestedVirtualization))
	}

	// Apply secure boot change
	if p.SecureBoot != nil {
		sbState := "Off"
		if *p.SecureBoot {
			sbState = "On"
		}
		script := fmt.Sprintf(`Set-VMFirmware -VMName "%s" -EnableSecureBoot %s -ErrorAction Stop`, vmName, sbState)
		if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
			return nil, fmt.Errorf("failed to set secure boot: %s", extractPSErrorDetail(err))
		}
		applied = append(applied, fmt.Sprintf("secure_boot=%t", *p.SecureBoot))
	}

	// Apply secure boot template change
	if p.SecureBootTemplate != nil {
		templateMap := map[string]string{
			"Windows": "MicrosoftWindows",
			"Linux":   "MicrosoftUEFICertificateAuthority",
			"Others":  "OpenSourceShieldedVM",
		}
		psTemplate, ok := templateMap[*p.SecureBootTemplate]
		if !ok {
			return nil, fmt.Errorf("invalid secure_boot_template: %q (valid: Windows, Linux, Others)", *p.SecureBootTemplate)
		}
		script := fmt.Sprintf(`Set-VMFirmware -VMName "%s" -SecureBootTemplate %s -ErrorAction Stop`, vmName, psTemplate)
		if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
			return nil, fmt.Errorf("failed to set secure boot template: %s", extractPSErrorDetail(err))
		}
		applied = append(applied, fmt.Sprintf("secure_boot_template=%s", *p.SecureBootTemplate))
	}

	// Apply automatic start action change
	if p.AutomaticStart != nil {
		validStartActions := map[string]bool{"Nothing": true, "StartIfRunning": true, "Start": true}
		if !validStartActions[*p.AutomaticStart] {
			return nil, fmt.Errorf("invalid automatic_start: %q (valid: Nothing, StartIfRunning, Start)", *p.AutomaticStart)
		}
		script := fmt.Sprintf(`Set-VM -VMName "%s" -AutomaticStartAction %s -ErrorAction Stop`, vmName, *p.AutomaticStart)
		if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
			return nil, fmt.Errorf("failed to set automatic start action: %s", extractPSErrorDetail(err))
		}
		applied = append(applied, fmt.Sprintf("automatic_start=%s", *p.AutomaticStart))
	}

	// Apply automatic start delay change
	if p.AutomaticStartDelay != nil {
		if *p.AutomaticStartDelay < 0 {
			return nil, fmt.Errorf("invalid automatic_start_delay: %d (must be >= 0)", *p.AutomaticStartDelay)
		}
		script := fmt.Sprintf(`Set-VM -VMName "%s" -AutomaticStartDelay %d -ErrorAction Stop`, vmName, *p.AutomaticStartDelay)
		if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
			return nil, fmt.Errorf("failed to set automatic start delay: %s", extractPSErrorDetail(err))
		}
		applied = append(applied, fmt.Sprintf("automatic_start_delay=%d", *p.AutomaticStartDelay))
	}

	// Apply automatic stop action change
	if p.AutomaticStop != nil {
		validStopActions := map[string]bool{"TurnOff": true, "ShutDown": true, "Save": true}
		if !validStopActions[*p.AutomaticStop] {
			return nil, fmt.Errorf("invalid automatic_stop: %q (valid: TurnOff, ShutDown, Save)", *p.AutomaticStop)
		}
		script := fmt.Sprintf(`Set-VM -VMName "%s" -AutomaticStopAction %s -ErrorAction Stop`, vmName, *p.AutomaticStop)
		if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
			return nil, fmt.Errorf("failed to set automatic stop action: %s", extractPSErrorDetail(err))
		}
		applied = append(applied, fmt.Sprintf("automatic_stop=%s", *p.AutomaticStop))
	}

	// Pre-check for disk operations: VM must not have snapshots, and a Generation 1
	// (BIOS) VM must be powered off - its disks hang off the IDE controller, which
	// does not support add/remove/online-resize while the VM is running.
	hasDiskOps := (p.AddDisk != nil && string(p.AddDisk) != "null" && string(p.AddDisk) != "{}") ||
		(p.EditDisk != nil && string(p.EditDisk) != "null" && string(p.EditDisk) != "{}") ||
		(p.RemoveDisk != nil && string(p.RemoveDisk) != "null" && string(p.RemoveDisk) != "{}")

	if hasDiskOps {
		snapScript := fmt.Sprintf(`
$vm = Get-VM -Name "%s" -ErrorAction Stop
$snaps = Get-VMSnapshot -VMName "%s" -ErrorAction SilentlyContinue
if ($snaps) { throw "VM has snapshots - remove all snapshots before modifying disks" }
if ($vm.Generation -eq 1 -and $vm.State -ne 'Off') {
    throw "This is a Generation 1 (BIOS) VM - it must be powered off to add, expand or remove disks (current state: $($vm.State))"
}
`, vmName, vmName)
		if _, err := hyperv.RunPowerShell(ctx, snapScript); err != nil {
			return nil, fmt.Errorf("disk operation blocked: %s", extractPSErrorDetail(err))
		}
	}

	// Add disk(s) - supports single object or array
	if p.AddDisk != nil && string(p.AddDisk) != "null" && string(p.AddDisk) != "{}" {
		var disks []VMEditDisk

		// Try array first, then single object
		if len(p.AddDisk) > 0 && p.AddDisk[0] == '[' {
			if err := json.Unmarshal(p.AddDisk, &disks); err != nil {
				return nil, fmt.Errorf("invalid add_disk array: %v", err)
			}
		} else {
			var single VMEditDisk
			if err := json.Unmarshal(p.AddDisk, &single); err != nil {
				return nil, fmt.Errorf("invalid add_disk: %v", err)
			}
			disks = []VMEditDisk{single}
		}

		// Pre-check: verify no disk files already exist
		for i, disk := range disks {
			if disk.Path == "" {
				return nil, fmt.Errorf("add_disk[%d].path is required", i)
			}
			if disk.SizeGB <= 0 {
				return nil, fmt.Errorf("add_disk[%d].size_gb must be greater than 0", i)
			}
		}

		// Disk space pre-check: ensure at least 3x total add_disk size is free
		var totalAddDiskGB float64
		for _, disk := range disks {
			totalAddDiskGB += disk.SizeGB
		}
		// Use the path of the first disk to determine the target volume
		if len(disks) > 0 {
			if err := d.checkDiskSpace(ctx, totalAddDiskGB, disks[0].Path); err != nil {
				return nil, err
			}
		}

		checkScript := `$ErrorActionPreference = 'Stop'; `
		for _, disk := range disks {
			checkScript += fmt.Sprintf(`if (Test-Path "%s") { throw "Disk file already exists: %s" }; `, disk.Path, disk.Path)
		}
		if _, err := hyperv.RunPowerShell(ctx, checkScript); err != nil {
			return nil, fmt.Errorf("add_disk pre-check failed: %s", extractPSErrorDetail(err))
		}

		totalDisks := len(disks)
		for i, disk := range disks {

			// Report progress
			percent := int(float64(i) / float64(totalDisks) * 100)
			d.reportProgress(ctx, taskID, TaskVMEdit, percent, fmt.Sprintf("Creating disk %d/%d: %s", i+1, totalDisks, disk.Path))

			diskType := "Dynamic"
			if disk.Type == "Fixed" {
				diskType = "Fixed"
			}

			sizeBytes := int64(disk.SizeGB * 1024 * 1024 * 1024)

			// Use a dedicated context with 10 min timeout per disk (Fixed disks can be very slow)
			diskCtx, diskCancel := context.WithTimeout(context.Background(), 10*time.Minute)

			script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
New-VHD -Path "%s" -SizeBytes %d -%s | Out-Null
Add-VMHardDiskDrive -VMName "%s" -Path "%s"
`, disk.Path, sizeBytes, diskType, vmName, disk.Path)

			_, err := hyperv.RunPowerShell(diskCtx, script)
			diskCancel()
			if err != nil {
				return nil, fmt.Errorf("failed to add disk[%d]: %v", i, err)
			}
			applied = append(applied, fmt.Sprintf("add_disk=%s", disk.Path))
		}
	}

	// Edit (expand) disk(s) - supports single object or array
	if p.EditDisk != nil && string(p.EditDisk) != "null" && string(p.EditDisk) != "{}" {
		var disks []VMEditDiskResize

		if len(p.EditDisk) > 0 && p.EditDisk[0] == '[' {
			if err := json.Unmarshal(p.EditDisk, &disks); err != nil {
				return nil, fmt.Errorf("invalid edit_disk array: %v", err)
			}
		} else {
			var single VMEditDiskResize
			if err := json.Unmarshal(p.EditDisk, &single); err != nil {
				return nil, fmt.Errorf("invalid edit_disk: %v", err)
			}
			disks = []VMEditDiskResize{single}
		}

		for i, disk := range disks {
			if disk.Path == "" {
				return nil, fmt.Errorf("edit_disk[%d].path is required", i)
			}
			if disk.SizeGB <= 0 {
				return nil, fmt.Errorf("edit_disk[%d].size_gb must be greater than 0", i)
			}

			// Report progress
			percent := int(float64(i) / float64(len(disks)) * 100)
			d.reportProgress(ctx, taskID, TaskVMEdit, percent, fmt.Sprintf("Expanding disk %d/%d: %s", i+1, len(disks), disk.Path))

			sizeBytes := int64(disk.SizeGB * 1024 * 1024 * 1024)

			// Resize-VHD only supports expanding (new size must be larger than current)
			script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
if (-not (Test-Path "%s")) { throw "Disk file not found: %s" }
$vhd = Get-VHD -Path "%s"
if (%d -le $vhd.Size) { throw "New size must be larger than current size ($([math]::Round($vhd.Size/1GB,2)) GB) for disk: %s" }
Resize-VHD -Path "%s" -SizeBytes %d
`, disk.Path, disk.Path, disk.Path, sizeBytes, disk.Path, disk.Path, sizeBytes)

			diskCtx, diskCancel := context.WithTimeout(context.Background(), 10*time.Minute)
			_, err := hyperv.RunPowerShell(diskCtx, script)
			diskCancel()
			if err != nil {
				return nil, fmt.Errorf("failed to expand disk[%d]: %v", i, err)
			}
			applied = append(applied, fmt.Sprintf("edit_disk=%s (%.1f GB)", disk.Path, disk.SizeGB))
		}
	}

	// Remove disk(s) - supports single object or array
	if p.RemoveDisk != nil && string(p.RemoveDisk) != "null" && string(p.RemoveDisk) != "{}" {
		var disks []VMRemoveDisk

		if len(p.RemoveDisk) > 0 && p.RemoveDisk[0] == '[' {
			if err := json.Unmarshal(p.RemoveDisk, &disks); err != nil {
				return nil, fmt.Errorf("invalid remove_disk array: %v", err)
			}
		} else {
			var single VMRemoveDisk
			if err := json.Unmarshal(p.RemoveDisk, &single); err != nil {
				return nil, fmt.Errorf("invalid remove_disk: %v", err)
			}
			disks = []VMRemoveDisk{single}
		}

		for i, disk := range disks {
			if disk.Path == "" {
				return nil, fmt.Errorf("remove_disk[%d].path is required", i)
			}

			// Report progress
			percent := int(float64(i) / float64(len(disks)) * 100)
			d.reportProgress(ctx, taskID, TaskVMEdit, percent, fmt.Sprintf("Removing disk %d/%d: %s", i+1, len(disks), disk.Path))

			// Detach disk from VM
			script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$drive = Get-VMHardDiskDrive -VMName "%s" | Where-Object { $_.Path -eq "%s" }
if (-not $drive) { throw "Disk not attached to VM: %s" }
Remove-VMHardDiskDrive -VMName "%s" -ControllerType $drive.ControllerType -ControllerNumber $drive.ControllerNumber -ControllerLocation $drive.ControllerLocation
`, vmName, disk.Path, disk.Path, vmName)

			if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
				return nil, fmt.Errorf("failed to detach disk[%d]: %s", i, extractPSErrorDetail(err))
			}

			// Optionally delete the VHD file from disk
			if disk.DeleteFromDisk {
				deleteScript := fmt.Sprintf(`
if (Test-Path "%s") { Remove-Item -Path "%s" -Force -ErrorAction Stop }
`, disk.Path, disk.Path)
				if _, err := hyperv.RunPowerShell(ctx, deleteScript); err != nil {
					d.logger.Warn("disk detached but failed to delete file", "path", disk.Path, "error", err)
				}
			}

			applied = append(applied, fmt.Sprintf("remove_disk=%s", disk.Path))
		}
	}

	// Add NIC(s) - supports single object or array
	if p.AddNIC != nil && string(p.AddNIC) != "null" && string(p.AddNIC) != "[]" {
		var nics []VMAddNIC

		if len(p.AddNIC) > 0 && p.AddNIC[0] == '[' {
			if err := json.Unmarshal(p.AddNIC, &nics); err != nil {
				return nil, fmt.Errorf("invalid add_nic array: %v", err)
			}
		} else {
			var single VMAddNIC
			if err := json.Unmarshal(p.AddNIC, &single); err != nil {
				return nil, fmt.Errorf("invalid add_nic: %v", err)
			}
			nics = []VMAddNIC{single}
		}

		for i, nic := range nics {
			if nic.Name == "" {
				return nil, fmt.Errorf("add_nic[%d].name is required", i)
			}

			script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
%s
Add-VMNetworkAdapter -VMName "%s" -Name "%s" -SwitchName $switchName
`, switchResolvePS(nic.SwitchName), vmName, escapePS(nic.Name))

			if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
				return nil, fmt.Errorf("failed to add NIC[%d] '%s': %s", i, nic.Name, extractPSErrorDetail(err))
			}

			switchNote := ""
			if nic.SwitchName != nil && *nic.SwitchName != "" {
				switchNote = fmt.Sprintf(" switch=%s", *nic.SwitchName)
			}

			// Set VLAN if specified
			if nic.VlanID != nil {
				vlanScript := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$adapter = Get-VMNetworkAdapter -VMName "%s" | Where-Object { $_.Name -eq "%s" }
if (-not $adapter) { throw "NIC '%s' not found after adding" }
Set-VMNetworkAdapterVlan -VMNetworkAdapter $adapter -Access -VlanId %d
`, vmName, escapePS(nic.Name), escapePS(nic.Name), *nic.VlanID)

				if _, err := hyperv.RunPowerShell(ctx, vlanScript); err != nil {
					return nil, fmt.Errorf("failed to set VLAN on NIC[%d] '%s': %s", i, nic.Name, extractPSErrorDetail(err))
				}
				applied = append(applied, fmt.Sprintf("add_nic=%s (vlan %d)%s", nic.Name, *nic.VlanID, switchNote))
			} else {
				applied = append(applied, fmt.Sprintf("add_nic=%s%s", nic.Name, switchNote))
			}
		}
	}

	// Edit NIC(s) - rename and/or change VLAN on existing NIC by nic_id
	if p.EditNIC != nil && string(p.EditNIC) != "null" && string(p.EditNIC) != "[]" {
		var nics []VMEditNIC

		if len(p.EditNIC) > 0 && p.EditNIC[0] == '[' {
			if err := json.Unmarshal(p.EditNIC, &nics); err != nil {
				return nil, fmt.Errorf("invalid edit_nic array: %v", err)
			}
		} else {
			var single VMEditNIC
			if err := json.Unmarshal(p.EditNIC, &single); err != nil {
				return nil, fmt.Errorf("invalid edit_nic: %v", err)
			}
			nics = []VMEditNIC{single}
		}

		for i, nic := range nics {
			if nic.NicID == "" {
				return nil, fmt.Errorf("edit_nic[%d].nic_id is required", i)
			}
			if nic.Name == nil && nic.VlanID == nil && (nic.SwitchName == nil || *nic.SwitchName == "") {
				return nil, fmt.Errorf("edit_nic[%d]: at least one of 'name', 'vlan_id' or 'switch_name' is required", i)
			}

			// Reconnect the adapter to a different virtual switch if requested
			if nic.SwitchName != nil && *nic.SwitchName != "" {
				script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
%s
$adapter = Get-VMNetworkAdapter -VMName "%s" | Where-Object { $_.Id -like "*\%s" }
if (-not $adapter) { throw "NIC with id '%s' not found on VM '%s'" }
Connect-VMNetworkAdapter -VMNetworkAdapter $adapter -SwitchName $switchName
`, switchResolvePS(nic.SwitchName), vmName, nic.NicID, nic.NicID, vmName)

				if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
					return nil, fmt.Errorf("failed to set switch on NIC[%d] '%s': %s", i, nic.NicID, extractPSErrorDetail(err))
				}
				applied = append(applied, fmt.Sprintf("edit_nic=%s (switch=%s)", nic.NicID, *nic.SwitchName))
			}

			// Rename NIC if name is specified
			if nic.Name != nil {
				script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$adapter = Get-VMNetworkAdapter -VMName "%s" | Where-Object { $_.Id -like "*\%s" }
if (-not $adapter) { throw "NIC with id '%s' not found on VM '%s'" }
Rename-VMNetworkAdapter -VMNetworkAdapter $adapter -NewName "%s"
`, vmName, nic.NicID, nic.NicID, vmName, escapePS(*nic.Name))

				if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
					return nil, fmt.Errorf("failed to rename NIC[%d] '%s': %s", i, nic.NicID, extractPSErrorDetail(err))
				}
				applied = append(applied, fmt.Sprintf("edit_nic=%s (name=%s)", nic.NicID, *nic.Name))
			}

			// Change VLAN if vlan_id is specified
			if nic.VlanID != nil {
				script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$adapter = Get-VMNetworkAdapter -VMName "%s" | Where-Object { $_.Id -like "*\%s" }
if (-not $adapter) { throw "NIC with id '%s' not found on VM '%s'" }
Set-VMNetworkAdapterVlan -VMNetworkAdapter $adapter -Access -VlanId %d
`, vmName, nic.NicID, nic.NicID, vmName, *nic.VlanID)

				if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
					return nil, fmt.Errorf("failed to set VLAN on NIC[%d] '%s': %s", i, nic.NicID, extractPSErrorDetail(err))
				}
				applied = append(applied, fmt.Sprintf("edit_nic=%s (vlan=%d)", nic.NicID, *nic.VlanID))
			}
		}
	}

	// Remove NIC(s) - remove NIC by nic_id
	if p.RemoveNIC != nil && string(p.RemoveNIC) != "null" && string(p.RemoveNIC) != "[]" {
		var nics []VMRemoveNIC

		if len(p.RemoveNIC) > 0 && p.RemoveNIC[0] == '[' {
			if err := json.Unmarshal(p.RemoveNIC, &nics); err != nil {
				return nil, fmt.Errorf("invalid remove_nic array: %v", err)
			}
		} else {
			var single VMRemoveNIC
			if err := json.Unmarshal(p.RemoveNIC, &single); err != nil {
				return nil, fmt.Errorf("invalid remove_nic: %v", err)
			}
			nics = []VMRemoveNIC{single}
		}

		for i, nic := range nics {
			if nic.NicID == "" {
				return nil, fmt.Errorf("remove_nic[%d].nic_id is required", i)
			}

			script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$adapter = Get-VMNetworkAdapter -VMName "%s" | Where-Object { $_.Id -like "*\%s" }
if (-not $adapter) { throw "NIC with id '%s' not found on VM '%s'" }
Remove-VMNetworkAdapter -VMNetworkAdapter $adapter
`, vmName, nic.NicID, nic.NicID, vmName)

			if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
				return nil, fmt.Errorf("failed to remove NIC[%d] '%s': %s", i, nic.NicID, extractPSErrorDetail(err))
			}
			applied = append(applied, fmt.Sprintf("remove_nic=%s", nic.NicID))
		}
	}

	if len(applied) == 0 {
		return nil, fmt.Errorf("no changes specified in payload")
	}

	return &VMEditResult{
		VMID:    p.VMID,
		VMName:  vmName,
		Applied: applied,
	}, nil
}

// mbToBytes converts an optional MiB value to an optional byte count.
func mbToBytes(mb *int) *int64 {
	if mb == nil {
		return nil
	}
	b := int64(*mb) * 1024 * 1024
	return &b
}

// psInt64OrExpr renders a literal int64 when v is set, else the PowerShell
// expression fallback (e.g. "$vm.MemoryStartup").
func psInt64OrExpr(v *int64, fallback string) string {
	if v == nil {
		return fallback
	}
	return fmt.Sprintf("%d", *v)
}

// psBoolOrExpr renders "$true"/"$false" when v is set, else the fallback.
func psBoolOrExpr(v *bool, fallback string) string {
	if v == nil {
		return fallback
	}
	if *v {
		return "$true"
	}
	return "$false"
}
