package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"ovc-agent/internal/hyperv"
)

// VMStatusPayload contains the VM ID for vm_status tasks
type VMStatusPayload struct {
	VMID   string `json:"vm_id"`
	VMName string `json:"vmName"`
}

func (d *Dispatcher) handleVMStatus(ctx context.Context, payload json.RawMessage) (*VMInventoryResult, error) {
	var p VMStatusPayload
	if payload != nil && string(payload) != "null" && string(payload) != "{}" {
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, fmt.Errorf("invalid payload: %v", err)
		}
	}

	// Determine how to find the VM
	var vmFilter string
	if p.VMID != "" {
		vmFilter = fmt.Sprintf(`Get-VM -Id "%s" -ErrorAction SilentlyContinue`, p.VMID)
	} else if p.VMName != "" {
		vmFilter = fmt.Sprintf(`Get-VM -Name "%s" -ErrorAction SilentlyContinue`, p.VMName)
	} else {
		// No filter, return all VMs
		return d.handleVMInventory(ctx)
	}

	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$v = %s
if (-not $v) {
    [Console]::Error.WriteLine("VM_NOT_FOUND")
    exit 1
}

# Check HA: verify node is in cluster and VM has a cluster resource
$isHA = $false
try {
    $node = Get-ClusterNode -Name $env:COMPUTERNAME -ErrorAction Stop
    if ($node.State -eq 'Up') {
        $res = Get-ClusterResource -VMId $v.Id -ErrorAction SilentlyContinue
        if ($res) { $isHA = $true }
    }
} catch {}

$nics = @()
try {
    $adapters = Get-VMNetworkAdapter -VMName $v.Name -ErrorAction SilentlyContinue
    foreach ($n in $adapters) {
        $ips = @()
        try { $ips = $n.IPAddresses } catch {}
        $nicIps = $ips | Where-Object { $_ }

        $ipString = ""
        if ($nicIps) {
            $ipString = $nicIps -join ", "
        }

        $vlanId = 0
        try {
            $vlanObj = $n | Get-VMNetworkAdapterVlan -ErrorAction SilentlyContinue
            if ($vlanObj) {
                $vlanId = $vlanObj.AccessVlanId
            }
        } catch {}

        $nicId = ""
        if ($n.Id) {
            $parts = $n.Id -split '\\'
            if ($parts.Count -ge 2) { $nicId = $parts[$parts.Count - 1] }
        }

        $nics += [pscustomobject]@{
            id          = $nicId
            name        = $n.Name
            networkId   = $n.SwitchName
            macAddress  = $n.MacAddress
            primary     = $false
            connected   = [bool]$n.SwitchName
            ipAddresses = "$ipString"
            vlanId      = $vlanId
        }
    }
} catch {}

# Get VHDs
$vhds = @()
try {
    $drives = Get-VMHardDiskDrive -VMName $v.Name -ErrorAction SilentlyContinue
    foreach ($d in $drives) {
        if ($d.Path) {
            $ctrl = "$($d.ControllerType) $($d.ControllerNumber):$($d.ControllerLocation)"
            try {
                $vhd = Get-VHD -Path $d.Path -ErrorAction SilentlyContinue
                if ($vhd) {
                    $vhds += [pscustomobject]@{
                        diskId     = "$($vhd.DiskIdentifier)"
                        path       = $vhd.Path
                        controller = $ctrl
                        format     = "$($vhd.VhdFormat)"
                        type       = "$($vhd.VhdType)"
                        sizeGb     = [math]::Round($vhd.Size / 1GB, 2)
                        usedBytes  = [int64]$vhd.FileSize
                    }
                } else {
                    $vhds += [pscustomobject]@{
                        diskId = ""; path = $d.Path; controller = $ctrl
                        format = "passthrough"; type = ""; sizeGb = 0; usedBytes = 0
                    }
                }
            } catch {}
        }
    }
} catch {}

# Get DVD/CD path
$dvdPath = $null
try {
    $dvd = Get-VMDvdDrive -VMName $v.Name -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Path
    if ($dvd) { $dvdPath = $dvd }
} catch {}

# Get snapshots
$snapshots = @()
try {
    $snaps = @(Get-VMSnapshot -VMName $v.Name -ErrorAction SilentlyContinue)
    foreach ($s in $snaps) {
        $parentId = $null
        $parentName = $null
        if ($s.ParentSnapshotId) { $parentId = "$($s.ParentSnapshotId)" }
        if ($s.ParentSnapshotName) { $parentName = $s.ParentSnapshotName }

        $snapshots += [pscustomobject]@{
            id                 = "$($s.Id)"
            name               = $s.Name
            creationTime       = $s.CreationTime.ToString("o")
            snapshotType       = "$($s.SnapshotType)"
            parentSnapshotId   = $parentId
            parentSnapshotName = $parentName
        }
    }
} catch {}

# Get firmware info (SecureBoot + SecureBootTemplate)
$secureBoot = $false
$secureBootTemplate = ""
try {
    $fw = Get-VMFirmware -VMName $v.Name -ErrorAction SilentlyContinue
    if ($fw) {
        $secureBoot = ($fw.SecureBoot -eq "On")
        $tpl = "$($fw.SecureBootTemplate)"
        switch ($tpl) {
            "MicrosoftWindows" { $secureBootTemplate = "Windows" }
            "MicrosoftUEFICertificateAuthority" { $secureBootTemplate = "Linux" }
            "OpenSourceShieldedVM" { $secureBootTemplate = "Others" }
            default { $secureBootTemplate = $tpl }
        }
    }
} catch {}

[pscustomobject]@{
    id               = "$($v.Id)"
    name             = $v.Name
    powerState       = "$($v.State)"
    firmware         = $(if ($v.Generation -eq 2) { "UEFI" } else { "BIOS" })
    cpu              = @{ vcpus = $v.ProcessorCount }
    memoryMb         = [math]::Round($v.MemoryStartup / 1MB)
    memoryMinBytes   = [int64]$v.MemoryMinimum
    memoryMaxBytes   = [int64]$v.MemoryMaximum
    memoryDynamic    = [bool]$v.DynamicMemoryEnabled
    state            = "$($v.State)"
    notes            = "$($v.Notes)"
    uptimeSec        = [int]$v.Uptime.TotalSeconds
    uptime           = "$($v.Uptime)"
    cpuUsagePct      = [int]$v.CPUUsage
    memoryConsumedMb = [math]::Round($v.MemoryDemand / 1MB)
    automaticStart   = "$($v.AutomaticStartAction)"
    automaticStartDelay = [int]$v.AutomaticStartDelay
    automaticStop    = "$($v.AutomaticStopAction)"
    path             = $v.Path
    creationTime     = $v.CreationTime.ToString("o")
    metricsEnabled   = [bool]$v.ResourceMeteringEnabled
    nics             = $nics
    vhds             = $vhds
    dvd              = $dvdPath
    snapshots        = $snapshots
    ha               = $isHA
    nestedVirtualization = [bool](Get-VMProcessor -VMName $v.Name -ErrorAction SilentlyContinue).ExposeVirtualizationExtensions
    secureBoot       = $secureBoot
    secureBootTemplate = $secureBootTemplate
} | ConvertTo-Json -Depth 5 -Compress
`, vmFilter)

	output, err := hyperv.RunPowerShell(ctx, script)
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "VM_NOT_FOUND") {
			identifier := p.VMID
			if identifier == "" {
				identifier = p.VMName
			}
			return nil, fmt.Errorf("VM not found: %s", identifier)
		}
		return nil, fmt.Errorf("failed to get VM status: %v", err)
	}

	output = trimPowerShellOutput(output)

	var vm VMInfo
	if err := json.Unmarshal(output, &vm); err != nil {
		return nil, fmt.Errorf("failed to parse VM status JSON: %v", err)
	}

	return &VMInventoryResult{VMs: []VMInfo{vm}}, nil
}
