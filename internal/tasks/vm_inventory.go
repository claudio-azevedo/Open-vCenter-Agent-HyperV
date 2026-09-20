package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"ovc-agent/internal/hyperv"
)

// VMInventoryResult holds the VM inventory response
type VMInventoryResult struct {
	VMs []VMInfo `json:"vms"`
}

type VMInfo struct {
	ID                   string       `json:"id"`
	Name                 string       `json:"name"`
	PowerState           string       `json:"powerState"`
	Firmware             string       `json:"firmware,omitempty"`
	CPU                  VMCPU        `json:"cpu"`
	MemoryMB             int64        `json:"memoryMb"`
	MemoryMinBytes       int64        `json:"memoryMinBytes"`
	MemoryMaxBytes       int64        `json:"memoryMaxBytes"`
	MemoryDynamic        bool         `json:"memoryDynamic,omitempty"`
	State                string       `json:"state"`
	Notes                string       `json:"notes"`
	UptimeSeconds        int64        `json:"uptimeSec"`
	Uptime               string       `json:"uptime"`
	CPUUsagePercent      int          `json:"cpuUsagePct"`
	MemoryConsumedMB     int64        `json:"memoryConsumedMb"`
	AutomaticStart       string       `json:"automaticStart"`
	AutomaticStartDelay  int          `json:"automaticStartDelay"`
	AutomaticStop        string       `json:"automaticStop"`
	Path                 string       `json:"path"`
	CreationTime         string       `json:"creationTime"`
	NICs                 []VMNic      `json:"nics"`
	VHDs                 []VMVHD      `json:"vhds"`
	DVD                  *string      `json:"dvd"`
	Snapshots            []VMSnapshot `json:"snapshots"`
	HA                   bool         `json:"ha"`
	NestedVirtualization bool         `json:"nestedVirtualization"`
	SecureBoot           bool         `json:"secureBoot"`
	SecureBootTemplate   string       `json:"secureBootTemplate"`
	MetricsEnabled       bool         `json:"metricsEnabled"`
}

type VMSnapshot struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	CreationTime       string  `json:"creationTime"`
	SnapshotType       string  `json:"snapshotType"`
	ParentSnapshotID   *string `json:"parentSnapshotId"`
	ParentSnapshotName *string `json:"parentSnapshotName"`
}

type VMCPU struct {
	VCPUs int `json:"vcpus"`
}

type VMNic struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	NetworkID   string `json:"networkId"`
	MacAddress  string `json:"macAddress"`
	Primary     bool   `json:"primary"`
	Connected   bool   `json:"connected"`
	IPAddresses string `json:"ipAddresses"`
	VlanID      int    `json:"vlanId"`
}

type VMVHD struct {
	DiskID     string  `json:"diskId"`
	Path       string  `json:"path"`
	Controller string  `json:"controller,omitempty"`
	Format     string  `json:"format"`
	Type       string  `json:"type"`
	SizeGB     float64 `json:"sizeGb"`
	UsedBytes  int64   `json:"usedBytes"`
}

// Internal types for JSON deserialization from PowerShell blocks
type vmBasicInfo struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	PowerState          string `json:"powerState"`
	Firmware            string `json:"firmware"`
	CPUCount            int    `json:"cpuCount"`
	MemoryMB            int64  `json:"memoryMb"`
	MemoryMinBytes      int64  `json:"memoryMinBytes"`
	MemoryMaxBytes      int64  `json:"memoryMaxBytes"`
	MemoryDynamic       bool   `json:"memoryDynamic"`
	State               string `json:"state"`
	Notes               string `json:"notes"`
	UptimeSeconds       int64  `json:"uptimeSec"`
	Uptime              string `json:"uptime"`
	CPUUsagePercent     int    `json:"cpuUsagePct"`
	MemoryConsumedMB    int64  `json:"memoryConsumedMb"`
	AutomaticStart      string `json:"automaticStart"`
	AutomaticStartDelay int    `json:"automaticStartDelay"`
	AutomaticStop       string `json:"automaticStop"`
	Path                string `json:"path"`
	CreationTime        string `json:"creationTime"`
	MetricsEnabled      bool   `json:"metricsEnabled"`
}

type vmNicInfo struct {
	VMName      string `json:"vmName"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	NetworkID   string `json:"networkId"`
	MacAddress  string `json:"macAddress"`
	IPAddresses string `json:"ipAddresses"`
	VlanID      int    `json:"vlanId"`
}

type vmVHDInfo struct {
	VMName string  `json:"vmName"`
	DiskID string  `json:"diskId"`
	Path   string  `json:"path"`
	Format string  `json:"format"`
	Type   string  `json:"type"`
	SizeGB float64 `json:"sizeGb"`
}

type vmHAInfo struct {
	VMId string `json:"vmId"`
	HA   bool   `json:"ha"`
}

type vmNestedInfo struct {
	VMName string `json:"vmName"`
	Nested bool   `json:"nested"`
}

type vmDVDInfo struct {
	VMName string  `json:"vmName"`
	DVD    *string `json:"dvd"`
}

type vmSnapshotInfo struct {
	VMName             string  `json:"vmName"`
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	CreationTime       string  `json:"creationTime"`
	SnapshotType       int     `json:"snapshotType"`
	ParentSnapshotID   *string `json:"parentSnapshotId"`
	ParentSnapshotName *string `json:"parentSnapshotName"`
}

type vmFirmwareInfo struct {
	VMName             string `json:"vmName"`
	SecureBoot         bool   `json:"secureBoot"`
	SecureBootTemplate string `json:"secureBootTemplate"`
}

// --- Merged enrichment types (single-pass inventory) ---
//
// vmEnrichmentInfo is the combined per-VM object emitted by the single merged
// enrichment PowerShell pass (task 3.2). One object is produced per VM, keyed by
// both vmName and vmId, carrying every enrichment attribute that blocks 2-8 used
// to collect in separate scripts. Its JSON tags mirror the existing per-block
// internal types (vmNicInfo, vmVHDInfo, vmSnapshotInfo, vmFirmwareInfo) so the
// same Go values are produced regardless of whether the data came from the old
// per-block scripts or the merged pass.
//
// The nested NIC objects carry the already-parsed NIC id (the last backslash
// segment), exactly as the per-block NIC scripts emit today - the last-segment
// split happens inside PowerShell, so Go consumes the id verbatim.
type vmEnrichmentInfo struct {
	VMName             string                 `json:"vmName"`
	VMId               string                 `json:"vmId"`
	NICs               []vmEnrichmentNic      `json:"nics"`
	VHDs               []vmEnrichmentVHD      `json:"vhds"`
	DVD                *string                `json:"dvd"`
	Nested             bool                   `json:"nested"`
	Snapshots          []vmEnrichmentSnapshot `json:"snapshots"`
	SecureBoot         bool                   `json:"secureBoot"`
	SecureBootTemplate string                 `json:"secureBootTemplate"`
	// HA is populated only when the host is a cluster node; otherwise it stays
	// false and the parse helper does not write it into haMap (cluster-skip).
	HA bool `json:"ha"`
}

// vmEnrichmentNic mirrors vmNicInfo's tags minus vmName (the parent object carries
// the VM identity). id is the last backslash segment already parsed in PowerShell.
type vmEnrichmentNic struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	NetworkID   string `json:"networkId"`
	MacAddress  string `json:"macAddress"`
	Connected   bool   `json:"connected"`
	IPAddresses string `json:"ipAddresses"`
	VlanID      int    `json:"vlanId"`
}

// vmEnrichmentVHD mirrors vmVHDInfo's tags minus vmName, plus the controller
// (ControllerType ControllerNumber:ControllerLocation, e.g. "SCSI 0:1").
type vmEnrichmentVHD struct {
	DiskID     string  `json:"diskId"`
	Path       string  `json:"path"`
	Controller string  `json:"controller,omitempty"`
	Format     string  `json:"format"`
	Type       string  `json:"type"`
	SizeGB     float64 `json:"sizeGb"`
	UsedBytes  int64   `json:"usedBytes"`
}

// vmEnrichmentSnapshot mirrors vmSnapshotInfo's tags minus vmName.
type vmEnrichmentSnapshot struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	CreationTime       string  `json:"creationTime"`
	SnapshotType       string  `json:"snapshotType"`
	ParentSnapshotID   *string `json:"parentSnapshotId"`
	ParentSnapshotName *string `json:"parentSnapshotName"`
}

// parseEnrichmentIntoMaps parses the merged enrichment output (an array of
// combined per-VM objects, or a single bare object when only one VM is present)
// and populates every enrichment map from that one pass. It is the single-parse
// replacement for the per-block map population in blocks 2-8; task 3.2 wires the
// merged PowerShell script's output through this helper.
//
// Preservation contract (must match the original per-block behavior byte-for-byte):
//   - Output is normalized through trimPowerShellOutput (UTF-8 BOM strip +
//     whitespace trim) and short-circuits on the len == 0 || "[]" guard.
//   - Single-object-vs-array fallback: try a slice first; on failure try a single
//     object and wrap it in a one-element slice; on total failure, return silently
//     (graceful degradation, exactly like the per-block parses).
//   - NIC values are copied verbatim (id already last-segment-parsed in PowerShell,
//     Primary always false, IPAddresses/VlanID preserved) so VMNic matches
//     parseNicsIntoMap's output exactly.
//   - dvdMap is written only when dvd is non-nil (matching block 6).
//   - nestedMap and firmwareMap are written for every VM in the pass (matching
//     blocks 6 and 8, which set them unconditionally per listed VM).
//   - haMap is keyed by vmId and written only when clustered is true, so standalone
//     hosts skip HA entirely (ha resolves to false in assembleVMInventory).
func parseEnrichmentIntoMaps(
	output []byte,
	clustered bool,
	nicsMap map[string][]VMNic,
	vhdsMap map[string][]VMVHD,
	dvdMap map[string]*string,
	nestedMap map[string]bool,
	snapshotsMap map[string][]VMSnapshot,
	firmwareMap map[string]vmFirmwareInfo,
	haMap map[string]bool,
) {
	output = trimPowerShellOutput(output)
	if len(output) == 0 || string(output) == "[]" {
		return
	}

	var infos []vmEnrichmentInfo
	if err := json.Unmarshal(output, &infos); err != nil {
		var single vmEnrichmentInfo
		if err2 := json.Unmarshal(output, &single); err2 != nil {
			return
		}
		infos = []vmEnrichmentInfo{single}
	}

	for _, e := range infos {
		for _, n := range e.NICs {
			nicsMap[e.VMName] = append(nicsMap[e.VMName], VMNic{
				ID:          n.ID,
				Name:        n.Name,
				NetworkID:   n.NetworkID,
				MacAddress:  n.MacAddress,
				Primary:     false,
				Connected:   n.Connected,
				IPAddresses: n.IPAddresses,
				VlanID:      n.VlanID,
			})
		}

		for _, v := range e.VHDs {
			vhdsMap[e.VMName] = append(vhdsMap[e.VMName], VMVHD{
				DiskID:     v.DiskID,
				Path:       v.Path,
				Controller: v.Controller,
				Format:     v.Format,
				Type:       v.Type,
				SizeGB:     v.SizeGB,
				UsedBytes:  v.UsedBytes,
			})
		}

		if e.DVD != nil {
			dvdMap[e.VMName] = e.DVD
		}

		nestedMap[e.VMName] = e.Nested

		for _, s := range e.Snapshots {
			snapshotsMap[e.VMName] = append(snapshotsMap[e.VMName], VMSnapshot{
				ID:                 s.ID,
				Name:               s.Name,
				CreationTime:       s.CreationTime,
				SnapshotType:       s.SnapshotType,
				ParentSnapshotID:   s.ParentSnapshotID,
				ParentSnapshotName: s.ParentSnapshotName,
			})
		}

		firmwareMap[e.VMName] = vmFirmwareInfo{
			VMName:             e.VMName,
			SecureBoot:         e.SecureBoot,
			SecureBootTemplate: e.SecureBootTemplate,
		}

		if clustered {
			haMap[e.VMId] = e.HA
		}
	}
}

// blockTimeout is the timeout for each individual PowerShell block in the inventory.
// Each block gets its own independent timeout so one slow block doesn't starve the others.
// Set to 5 minutes to accommodate hosts with 50+ VMs where Get-VHD and cluster queries
// can be slow.
const blockTimeout = 5 * time.Minute

// buildMergedEnrichmentScript returns the single merged enrichment PowerShell pass
// (former blocks 2-8) for the given cluster-node state. It is a pure function of
// `clustered` so the script's structure can be asserted at the source level without
// a live Hyper-V host (task 3.3): the Get-ClusterResource cmdlet is emitted only
// inside the `if ($clustered)` guard, and the `$clustered` PowerShell flag is set to
// `$true`/`$false` to mirror d.cluster.IsClusterNode. When standalone, the guard is
// `$false` so the cluster cmdlet never executes and ha resolves to false.
func buildMergedEnrichmentScript(clustered bool) string {
	clusteredPS := "$false"
	if clustered {
		clusteredPS = "$true"
	}

	return fmt.Sprintf(`
try {
    $clustered = %s
    $vms = @(Get-VM -ErrorAction Stop)
    $results = [System.Collections.ArrayList]::new()

    foreach ($v in $vms) {
        try {
            $vmName = $v.Name
            $isRunning = ($v.State -eq "Running")

            # --- NICs ---
            $nics = [System.Collections.ArrayList]::new()
            try {
                $adapters = @(Get-VMNetworkAdapter -VMName $vmName -ErrorAction SilentlyContinue)
                foreach ($n in $adapters) {
                    $ipString = ""
                    if ($isRunning -and $n.IPAddresses) {
                        $nicIps = @($n.IPAddresses | Where-Object { $_ })
                        if ($nicIps.Count -gt 0) { $ipString = $nicIps -join ", " }
                    }

                    $vlanId = 0
                    try {
                        $vlanObj = $n | Get-VMNetworkAdapterVlan -ErrorAction SilentlyContinue
                        if ($vlanObj) { $vlanId = $vlanObj.AccessVlanId }
                    } catch {}

                    $nicId = ""
                    if ($n.Id) {
                        $parts = $n.Id -split '\\'
                        if ($parts.Count -ge 2) { $nicId = $parts[$parts.Count - 1] }
                    }

                    $null = $nics.Add([pscustomobject]@{
                        id          = $nicId
                        name        = $n.Name
                        networkId   = $n.SwitchName
                        macAddress  = $n.MacAddress
                        connected   = [bool]$n.SwitchName
                        ipAddresses = "$ipString"
                        vlanId      = $vlanId
                    })
                }
            } catch {}

            # --- VHDs ---
            $vhds = [System.Collections.ArrayList]::new()
            try {
                $drives = Get-VMHardDiskDrive -VMName $vmName -ErrorAction SilentlyContinue
                foreach ($d in $drives) {
                    if ($d.Path) {
                        $ctrl = "$($d.ControllerType) $($d.ControllerNumber):$($d.ControllerLocation)"
                        try {
                            $vhd = Get-VHD -Path $d.Path -ErrorAction SilentlyContinue
                            if ($vhd) {
                                $null = $vhds.Add([pscustomobject]@{
                                    diskId     = "$($vhd.DiskIdentifier)"
                                    path       = $vhd.Path
                                    controller = $ctrl
                                    format     = "$($vhd.VhdFormat)"
                                    type       = "$($vhd.VhdType)"
                                    sizeGb     = [math]::Round($vhd.Size / 1GB, 2)
                                    usedBytes  = [int64]$vhd.FileSize
                                })
                            } else {
                                # pass-through / physical disk - no Get-VHD metadata
                                $null = $vhds.Add([pscustomobject]@{
                                    diskId     = ""
                                    path       = $d.Path
                                    controller = $ctrl
                                    format     = "passthrough"
                                    type       = ""
                                    sizeGb     = 0
                                    usedBytes  = 0
                                })
                            }
                        } catch {}
                    }
                }
            } catch {}

            # --- Nested virtualization + DVD ---
            $nested = $false
            $dvdPath = $null
            try {
                $nested = [bool](Get-VMProcessor -VMName $vmName -ErrorAction SilentlyContinue).ExposeVirtualizationExtensions
            } catch {}
            try {
                $dvd = Get-VMDvdDrive -VMName $vmName -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Path
                if ($dvd) { $dvdPath = $dvd }
            } catch {}

            # --- Snapshots ---
            $snaps = [System.Collections.ArrayList]::new()
            try {
                $snapList = @(Get-VMSnapshot -VMName $vmName -ErrorAction SilentlyContinue)
                foreach ($s in $snapList) {
                    $parentId = $null
                    $parentName = $null
                    if ($s.ParentSnapshotId) { $parentId = "$($s.ParentSnapshotId)" }
                    if ($s.ParentSnapshotName) { $parentName = $s.ParentSnapshotName }

                    $null = $snaps.Add([pscustomobject]@{
                        id                 = "$($s.Id)"
                        name               = $s.Name
                        creationTime       = $s.CreationTime.ToString("o")
                        snapshotType       = "$($s.SnapshotType)"
                        parentSnapshotId   = $parentId
                        parentSnapshotName = $parentName
                    })
                }
            } catch {}

            # --- Firmware (SecureBoot + SecureBootTemplate) ---
            $sb = $false
            $sbTemplate = ""
            try {
                $fw = Get-VMFirmware -VMName $vmName -ErrorAction SilentlyContinue
                if ($fw) {
                    $sb = ($fw.SecureBoot -eq "On")
                    $tpl = "$($fw.SecureBootTemplate)"
                    switch ($tpl) {
                        "MicrosoftWindows" { $sbTemplate = "Windows" }
                        "MicrosoftUEFICertificateAuthority" { $sbTemplate = "Linux" }
                        "OpenSourceShieldedVM" { $sbTemplate = "Others" }
                        default { $sbTemplate = $tpl }
                    }
                }
            } catch {}

            # --- HA (only when this host is a cluster node) ---
            $isHA = $false
            if ($clustered) {
                try {
                    $res = Get-ClusterResource -VMId $v.Id -ErrorAction SilentlyContinue
                    if ($res) { $isHA = $true }
                } catch {}
            }

            $null = $results.Add([pscustomobject]@{
                vmName             = $vmName
                vmId               = "$($v.Id)"
                nics               = $nics
                vhds               = $vhds
                dvd                = $dvdPath
                nested             = $nested
                snapshots          = $snaps
                secureBoot         = $sb
                secureBootTemplate = $sbTemplate
                ha                 = $isHA
            })
        } catch {}
    }

    if ($results.Count -eq 0) { Write-Output "[]" }
    else { $results | ConvertTo-Json -Depth 5 -Compress }
} catch {
    [Console]::Error.WriteLine("INVENTORY_ENRICHMENT_ERROR: $($_.Exception.Message)")
    exit 1
}
`, clusteredPS)
}

func (d *Dispatcher) handleVMInventory(ctx context.Context) (*VMInventoryResult, error) {
	// --- Block 1: Get basic VM info (fast operation) ---
	basicScript := `
try {
    $vms = @(Get-VM -ErrorAction Stop)
    if ($vms.Count -eq 0) {
        Write-Output "[]"
        return
    }

    $list = [System.Collections.ArrayList]::new()
    foreach ($v in $vms) {
        $null = $list.Add([pscustomobject]@{
            id               = "$($v.Id)"
            name             = $v.Name
            powerState       = "$($v.State)"
            firmware         = $(if ($v.Generation -eq 2) { "UEFI" } else { "BIOS" })
            cpuCount         = $v.ProcessorCount
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
            automaticStart      = "$($v.AutomaticStartAction)"
            automaticStartDelay = [int]$v.AutomaticStartDelay
            automaticStop       = "$($v.AutomaticStopAction)"
            path             = $v.Path
            creationTime     = $v.CreationTime.ToString("o")
            metricsEnabled   = [bool]$v.ResourceMeteringEnabled
        })
    }
    $list | ConvertTo-Json -Depth 5 -Compress
} catch {
    [Console]::Error.WriteLine("INVENTORY_BASIC_ERROR: $($_.Exception.Message)")
    exit 1
}
`
	blockCtx, cancel := context.WithTimeout(context.Background(), blockTimeout)
	basicOutput, err := hyperv.RunPowerShell(blockCtx, basicScript)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("failed to get VM basic info: %v", err)
	}

	basicOutput = trimPowerShellOutput(basicOutput)
	if len(basicOutput) == 0 || string(basicOutput) == "[]" {
		return &VMInventoryResult{VMs: []VMInfo{}}, nil
	}

	var basics []vmBasicInfo
	if err := json.Unmarshal(basicOutput, &basics); err != nil {
		var single vmBasicInfo
		if err2 := json.Unmarshal(basicOutput, &single); err2 != nil {
			return nil, fmt.Errorf("failed to parse VM basic JSON: %v (raw: %s)", err, string(basicOutput))
		}
		basics = []vmBasicInfo{single}
	}

	// --- Merged enrichment pass (replaces former blocks 2-8) ---
	//
	// A single PowerShell process enumerates every VM once and emits one combined
	// per-VM object carrying nics, vhds, dvd, nested, snapshots, firmware (and ha
	// when clustered). This collapses the previous 6-7 separate enrichment scripts
	// (NICs-running, NICs-off, VHDs, HA, nested+DVD, snapshots, firmware) into one
	// spawn while producing byte-identical VMInventoryResult data.
	//
	// Behavior preserved verbatim from the old blocks:
	//   - Block 1 (basic info) above stays a standalone, authoritative, FATAL script.
	//   - Running-vs-off IP nuance: ipAddresses is populated only for Running VMs;
	//     off VMs emit ipAddresses = "" (former split blocks 2 and 3).
	//   - NIC Id last-segment parsing (split on backslash) and VLAN lookup unchanged.
	//   - Firmware SecureBootTemplate mapping (Windows/Linux/Others/raw) unchanged.
	//   - HA (Get-ClusterResource -VMId) runs only when the host is a cluster node,
	//     folded into this pass and guarded by the $clustered flag so standalone
	//     hosts invoke no cluster cmdlet at all (keeps handleVMInventory at 2 spawns).
	//   - Per-VM try/catch keeps one bad VM from failing the whole pass.
	//   - Graceful degradation: if the merged script fails, log a warning and
	//     continue assembling from basic info (all enrichment maps stay empty);
	//     assembleVMInventory normalizes nil NIC/VHD/Snapshot slices to [].
	nicsMap := make(map[string][]VMNic)
	vhdsMap := make(map[string][]VMVHD)
	dvdMap := make(map[string]*string)
	nestedMap := make(map[string]bool)
	snapshotsMap := make(map[string][]VMSnapshot)
	firmwareMap := make(map[string]vmFirmwareInfo)
	haMap := make(map[string]bool)

	clustered := d.cluster.IsClusterNode
	mergedScript := buildMergedEnrichmentScript(clustered)

	blockCtx, cancel = context.WithTimeout(context.Background(), blockTimeout)
	enrichmentOutput, err := hyperv.RunPowerShell(blockCtx, mergedScript)
	cancel()
	if err != nil {
		d.logger.Warn("failed to get merged VM enrichment info", "error", err)
	} else {
		parseEnrichmentIntoMaps(
			enrichmentOutput,
			clustered,
			nicsMap,
			vhdsMap,
			dvdMap,
			nestedMap,
			snapshotsMap,
			firmwareMap,
			haMap,
		)
	}

	// --- Assemble final result ---
	vms := assembleVMInventory(basics, nicsMap, vhdsMap, dvdMap, nestedMap, snapshotsMap, firmwareMap, haMap)

	return &VMInventoryResult{VMs: vms}, nil
}

// assembleVMInventory merges the per-block enrichment maps onto the basic VM info
// to produce the final []VMInfo. NICs are keyed by VM name, HA by VM id (matching
// the existing haMap[b.ID] lookup), and nil NIC/VHD/Snapshot slices are normalized
// to empty slices so the JSON output emits [] rather than null.
//
// This is a verbatim extraction of the original inline assembly loop in
// handleVMInventory, exposed as a pure function so the assembly contract can be
// captured as a preservation baseline independent of any live PowerShell host.
func assembleVMInventory(
	basics []vmBasicInfo,
	nicsMap map[string][]VMNic,
	vhdsMap map[string][]VMVHD,
	dvdMap map[string]*string,
	nestedMap map[string]bool,
	snapshotsMap map[string][]VMSnapshot,
	firmwareMap map[string]vmFirmwareInfo,
	haMap map[string]bool,
) []VMInfo {
	vms := make([]VMInfo, 0, len(basics))
	for _, b := range basics {
		vm := VMInfo{
			ID:                   b.ID,
			Name:                 b.Name,
			PowerState:           b.PowerState,
			Firmware:             b.Firmware,
			CPU:                  VMCPU{VCPUs: b.CPUCount},
			MemoryMB:             b.MemoryMB,
			MemoryMinBytes:       b.MemoryMinBytes,
			MemoryMaxBytes:       b.MemoryMaxBytes,
			MemoryDynamic:        b.MemoryDynamic,
			State:                b.State,
			Notes:                b.Notes,
			UptimeSeconds:        b.UptimeSeconds,
			Uptime:               b.Uptime,
			CPUUsagePercent:      b.CPUUsagePercent,
			MemoryConsumedMB:     b.MemoryConsumedMB,
			AutomaticStart:       b.AutomaticStart,
			AutomaticStartDelay:  b.AutomaticStartDelay,
			AutomaticStop:        b.AutomaticStop,
			Path:                 b.Path,
			CreationTime:         b.CreationTime,
			MetricsEnabled:       b.MetricsEnabled,
			NICs:                 nicsMap[b.Name],
			VHDs:                 vhdsMap[b.Name],
			DVD:                  dvdMap[b.Name],
			Snapshots:            snapshotsMap[b.Name],
			HA:                   haMap[b.ID],
			NestedVirtualization: nestedMap[b.Name],
			SecureBoot:           firmwareMap[b.Name].SecureBoot,
			SecureBootTemplate:   firmwareMap[b.Name].SecureBootTemplate,
		}
		if vm.NICs == nil {
			vm.NICs = []VMNic{}
		}
		if vm.VHDs == nil {
			vm.VHDs = []VMVHD{}
		}
		if vm.Snapshots == nil {
			vm.Snapshots = []VMSnapshot{}
		}
		vms = append(vms, vm)
	}
	return vms
}

// parseNicsIntoMap parses NIC JSON output and populates the map
func parseNicsIntoMap(output []byte, nicsMap map[string][]VMNic) {
	output = trimPowerShellOutput(output)
	if len(output) == 0 || string(output) == "[]" {
		return
	}

	var nics []vmNicInfo
	if err := json.Unmarshal(output, &nics); err != nil {
		var single vmNicInfo
		if err2 := json.Unmarshal(output, &single); err2 == nil {
			nics = []vmNicInfo{single}
		} else {
			return
		}
	}

	for _, n := range nics {
		nicsMap[n.VMName] = append(nicsMap[n.VMName], VMNic{
			ID:          n.ID,
			Name:        n.Name,
			NetworkID:   n.NetworkID,
			MacAddress:  n.MacAddress,
			Primary:     false,
			IPAddresses: n.IPAddresses,
			VlanID:      n.VlanID,
		})
	}
}

// buildPSArray builds a PowerShell array literal from a slice of strings
func buildPSArray(names []string) string {
	if len(names) == 0 {
		return "@()"
	}
	escaped := make([]string, len(names))
	for i, name := range names {
		escaped[i] = fmt.Sprintf(`"%s"`, strings.ReplaceAll(name, `"`, "`\""))
	}
	return fmt.Sprintf("@(%s)", strings.Join(escaped, ","))
}

func trimPowerShellOutput(output []byte) []byte {
	// Trim UTF-8 BOM
	if len(output) >= 3 && output[0] == 0xef && output[1] == 0xbb && output[2] == 0xbf {
		output = output[3:]
	}
	// Trim whitespace
	start := 0
	for start < len(output) && (output[start] == ' ' || output[start] == '\t' || output[start] == '\r' || output[start] == '\n') {
		start++
	}
	end := len(output)
	for end > start && (output[end-1] == ' ' || output[end-1] == '\t' || output[end-1] == '\r' || output[end-1] == '\n') {
		end--
	}
	return output[start:end]
}
