package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"

	"ovc-agent/internal/hostinfo"
	"ovc-agent/internal/hyperv"
)

// HardwareInventoryResult is the raw host inventory the collector assembles
// (PowerShell + native Go). On the wire it is re-shaped by MarshalJSON into the
// canonical HostHardwareInventory the backend expects - see marshal_hardware.go.
type HardwareInventoryResult struct {
	Hostname           string             `json:"hostname"`
	OSName             string             `json:"osName"`
	OSVersion          string             `json:"osVersion"`
	LogicalProcessors  int                `json:"logicalProcessors"`
	CPUSockets         int                `json:"cpuSockets"`
	CPUCores           int                `json:"cpuCores"`
	MemoryCapacityMB   float64            `json:"memoryCapacityMB"`
	HWManufacturer     string             `json:"hwManufacturer"`
	HWModel            string             `json:"hwModel"`
	CPUModel           string             `json:"cpuModel"`
	CPULoadPercent     float64            `json:"cpuLoadPercent"`
	MemoryUsagePercent float64            `json:"memoryUsagePercent"`
	LastBootTime       string             `json:"lastBootTime"`
	UptimeDays         float64            `json:"uptimeDays"`
	TotalVMs           int                `json:"totalVMs"`
	RunningVMs         int                `json:"runningVMs"`
	DefaultVMPath      string             `json:"defaultVmPath"`
	DefaultVHDPath     string             `json:"defaultVhdPath"`
	IsCluster          bool               `json:"isCluster"`
	ClusterName        string             `json:"clusterName,omitempty"`
	ClusterState       string             `json:"clusterState"`
	ClusterNodes       []string           `json:"clusterNodes"`
	AutoBalancerMode   *int               `json:"autoBalancerMode,omitempty"`
	AutoBalancerLevel  *int               `json:"autoBalancerLevel,omitempty"`
	DiskC              DiskInfo           `json:"diskC"`
	Storage            []StorageInfo      `json:"storage"`
	NetAdapters        []NetAdapterInfo   `json:"netAdapters"`
	VSwitches          []VSwitchInfo      `json:"vSwitches"`
	ISOs               []hostinfo.ISOInfo `json:"isos"`
}

// NetAdapterInfo is one physical host NIC (reported whether connected or not).
type NetAdapterInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	MAC         string `json:"mac"`
	SpeedBps    int64  `json:"speedBps"`
	Connected   bool   `json:"connected"`
	Status      string `json:"status"`
}

// VSwitchInfo is one Hyper-V virtual switch.
type VSwitchInfo struct {
	Name       string `json:"name"`
	Type       string `json:"type"`       // External | Internal | Private
	NetAdapter string `json:"netAdapter"` // physical uplink (External only)
}

// DiskInfo holds basic disk space information
type DiskInfo struct {
	TotalGB float64 `json:"totalGb"`
	FreeGB  float64 `json:"freeGb"`
}

// StorageInfo holds storage volume information (CSV or local disk D:)
type StorageInfo struct {
	Name        string  `json:"name"`
	Path        string  `json:"path"`
	TotalGB     float64 `json:"totalGb"`
	FreeGB      float64 `json:"freeGb"`
	PercentFree float64 `json:"percentFree"`
}

// buildStorageInfo assembles a StorageInfo with rounded GB values and percentFree.
func buildStorageInfo(name, path string, disk *hostinfo.DiskSpaceInfo) StorageInfo {
	totalGB := math.Round(disk.TotalGB*100) / 100
	freeGB := math.Round(disk.FreeGB*100) / 100
	percentFree := 0.0
	if totalGB > 0 {
		percentFree = math.Round((freeGB/totalGB)*1000) / 10
	}
	return StorageInfo{
		Name:        name,
		Path:        path,
		TotalGB:     totalGB,
		FreeGB:      freeGB,
		PercentFree: percentFree,
	}
}

// storageVolumeUpper returns the upper-cased volume/drive of a path (e.g. "E:").
func storageVolumeUpper(p string) string {
	return strings.ToUpper(filepath.VolumeName(p))
}

// decodeArrayOrSingle unmarshals a PowerShell `ConvertTo-Json` field that may be
// an array, a single object (when one element), or null/absent. Always returns a
// non-nil slice so the JSON re-marshal emits `[]` rather than `null`.
func decodeArrayOrSingle[T any](raw json.RawMessage) []T {
	if len(raw) == 0 || string(raw) == "null" {
		return []T{}
	}
	var list []T
	if err := json.Unmarshal(raw, &list); err == nil {
		if list == nil {
			return []T{}
		}
		return list
	}
	var single T
	if err := json.Unmarshal(raw, &single); err == nil {
		return []T{single}
	}
	return []T{}
}

func (d *Dispatcher) handleHardwareInventory(ctx context.Context) (*HardwareInventoryResult, error) {
	// Use cached cluster membership from startup detection
	isCluster := d.cluster.IsClusterNode

	// --- Native Go: disk C:, uptime, ISOs (no PowerShell overhead) ---
	var diskC DiskInfo
	if diskInfo, err := hostinfo.GetDiskSpace("C:\\"); err == nil {
		diskC = DiskInfo{
			TotalGB: math.Round(diskInfo.TotalGB*100) / 100,
			FreeGB:  math.Round(diskInfo.FreeGB*100) / 100,
		}
	}

	uptimeDays, lastBootTime := hostinfo.HostUptime()
	uptimeDays = math.Round(uptimeDays*100) / 100
	lastBootTimeStr := lastBootTime.Format("2006-01-02T15:04:05.0000000-07:00")

	// --- PowerShell: everything that requires Hyper-V/Cluster cmdlets ---
	script := fmt.Sprintf(`
try { $ci = Get-ComputerInfo } catch { $ci = $null }
$os = Get-CimInstance Win32_OperatingSystem
$VMhost = Get-VMHost
$procs = @(Get-CimInstance Win32_Processor)
$cpu = ($procs | Measure-Object -Property LoadPercentage -Average).Average
$cpuSockets = $procs.Count
$cpuCores = ($procs | Measure-Object -Property NumberOfCores -Sum).Sum
$vms = Get-VM
$running = ($vms | Where-Object {$_.State -eq 'Running'}).Count
$memPerc = [math]::Round((($os.TotalVisibleMemorySize - $os.FreePhysicalMemory) / $os.TotalVisibleMemorySize) * 100, 2)

# CPU model as string
$cpuModelStr = ""
if ($ci.CsProcessors) {
    $cpuModelStr = ($ci.CsProcessors | ForEach-Object { $_.Name }) -join ", "
}

# Cluster state (refreshed every run, only if node is clustered)
$isCluster = $%v
$clusterState = "Not Clustered"
$clusterNodes = @()
$clusterName = ""
$autoBalancerMode = $null
$autoBalancerLevel = $null
if ($isCluster) {
    try {
        $nodes = Get-ClusterNode -ErrorAction Stop
        $clusterNodes = @($nodes | ForEach-Object { $_.Name })
        $localNode = $nodes | Where-Object { $_.Name -eq $env:COMPUTERNAME }
        if ($localNode) { $clusterState = $localNode.State.ToString() }
    } catch {
        $clusterState = "Error"
    }
    try {
        $clusterObj = Get-Cluster -ErrorAction Stop
        $clusterName = $clusterObj.Name
        $autoBalancerMode = [int]$clusterObj.AutoBalancerMode
        $autoBalancerLevel = [int]$clusterObj.AutoBalancerLevel
    } catch {}
}

# Storage: CSVs if cluster, otherwise handled by Go (standalone disk D:)
$storage = @()
if ($isCluster) {
    try {
        $csvs = Get-ClusterSharedVolume -ErrorAction Stop
        foreach ($csv in $csvs) {
            $volInfo = $csv.SharedVolumeInfo.Partition
            $storage += [PSCustomObject]@{
                name        = $csv.Name
                path        = $csv.SharedVolumeInfo.FriendlyVolumeName
                totalGb     = [math]::Round($volInfo.Size / 1GB, 2)
                freeGb      = [math]::Round($volInfo.FreeSpace / 1GB, 2)
                percentFree = [math]::Round(($volInfo.FreeSpace / $volInfo.Size) * 100, 1)
            }
        }
    } catch {}
}

# Physical NICs - every adapter, connected or not. Speed in bits/sec.
$netAdapters = @()
try {
    foreach ($na in @(Get-NetAdapter -Physical -ErrorAction Stop)) {
        $netAdapters += [PSCustomObject]@{
            name        = $na.Name
            description = $na.InterfaceDescription
            mac         = $na.MacAddress
            speedBps    = [int64]$na.Speed
            connected   = ($na.Status -eq 'Up')
            status      = "$($na.Status)"
        }
    }
} catch {}

# Hyper-V virtual switches.
$vSwitches = @()
try {
    foreach ($sw in @(Get-VMSwitch -ErrorAction Stop)) {
        $vSwitches += [PSCustomObject]@{
            name       = $sw.Name
            type       = "$($sw.SwitchType)"
            netAdapter = "$($sw.NetAdapterInterfaceDescription)"
        }
    }
} catch {}

[PSCustomObject]@{
    hostname           = $ci.CsName
    osName             = $ci.OsName
    osVersion          = $ci.OsVersion
    logicalProcessors  = $VMhost.LogicalProcessorCount
    cpuSockets         = $cpuSockets
    cpuCores           = $cpuCores
    memoryCapacityMB   = [math]::Round($os.TotalVisibleMemorySize / 1024, 2)
    hwManufacturer     = $ci.CsManufacturer
    hwModel            = $ci.CsModel
    cpuModel           = $cpuModelStr
    cpuLoadPercent     = $cpu
    memoryUsagePercent = $memPerc
    totalVMs           = $vms.Count
    runningVMs         = $running
    defaultVmPath      = $VMhost.VirtualMachinePath
    defaultVhdPath     = $VMhost.VirtualHardDiskPath
    isCluster          = $isCluster
    clusterName        = $clusterName
    clusterState       = $clusterState
    clusterNodes       = $clusterNodes
    autoBalancerMode   = $autoBalancerMode
    autoBalancerLevel  = $autoBalancerLevel
    storage            = $storage
    netAdapters        = $netAdapters
    vSwitches          = $vSwitches
} | ConvertTo-Json -Depth 4 -Compress
`, isCluster)

	output, err := hyperv.RunPowerShell(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("failed to get hardware inventory: %v", err)
	}

	output = trimPowerShellOutput(output)

	if len(output) == 0 {
		return nil, fmt.Errorf("hardware inventory returned empty output")
	}

	// Parse into a raw structure first to handle storage array/object
	var raw struct {
		HardwareInventoryResult
		StorageRaw     json.RawMessage `json:"storage"`
		NetAdaptersRaw json.RawMessage `json:"netAdapters"`
		VSwitchesRaw   json.RawMessage `json:"vSwitches"`
	}
	if err := json.Unmarshal(output, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse hardware inventory JSON: %v (raw: %s)", err, string(output))
	}

	result := raw.HardwareInventoryResult
	result.NetAdapters = decodeArrayOrSingle[NetAdapterInfo](raw.NetAdaptersRaw)
	result.VSwitches = decodeArrayOrSingle[VSwitchInfo](raw.VSwitchesRaw)

	// Ensure clusterNodes is never null in JSON output
	if result.ClusterNodes == nil {
		result.ClusterNodes = []string{}
	}

	// Handle single object vs array for storage (PowerShell quirk)
	if raw.StorageRaw != nil && string(raw.StorageRaw) != "null" {
		var storageList []StorageInfo
		if err := json.Unmarshal(raw.StorageRaw, &storageList); err != nil {
			var single StorageInfo
			if err2 := json.Unmarshal(raw.StorageRaw, &single); err2 == nil {
				storageList = []StorageInfo{single}
			}
		}
		result.Storage = storageList
	}
	if result.Storage == nil {
		result.Storage = []StorageInfo{}
	}

	// Standalone hosts: get disk D: info via native Go (no PowerShell)
	if !isCluster && len(result.Storage) == 0 {
		if diskD, err := hostinfo.GetDiskSpace("D:\\"); err == nil {
			// Use the volume label as the display name, falling back to "D:" when unlabeled.
			name := hostinfo.VolumeDisplayName(`D:\`)
			result.Storage = []StorageInfo{buildStorageInfo(name, `D:\`, diskD)}
		}
	}

	// Standalone hosts: append configured additional VM storage disks.
	// Skip any that live on the same drive as the Hyper-V default VHD path, and
	// any drive already present in the storage list (e.g. the default D:).
	if !isCluster && len(d.additionalStorage) > 0 {
		seenVolumes := make(map[string]bool)
		for _, s := range result.Storage {
			seenVolumes[storageVolumeUpper(s.Path)] = true
		}
		defaultVol := storageVolumeUpper(result.DefaultVHDPath)

		for _, storagePath := range d.additionalStorage {
			vol := storageVolumeUpper(storagePath)
			if vol == "" || vol == defaultVol || seenVolumes[vol] {
				continue
			}
			diskInfo, err := hostinfo.GetDiskSpace(storagePath)
			if err != nil {
				d.logger.Warn("failed to get disk space for additional storage", "path", storagePath, "error", err)
				continue
			}
			name := hostinfo.VolumeDisplayName(storagePath)
			result.Storage = append(result.Storage, buildStorageInfo(name, storagePath, diskInfo))
			seenVolumes[vol] = true
		}
	}

	// --- Native Go: inject disk C:, uptime, and ISO data (no PowerShell) ---
	result.DiskC = diskC
	result.UptimeDays = uptimeDays
	result.LastBootTime = lastBootTimeStr

	// --- Native Go: ISO inventory via os.ReadDir + crypto/md5 ---
	if d.localISOPath != "" {
		isos, err := hostinfo.ScanISOs(d.localISOPath)
		if err != nil {
			d.logger.Warn("failed to scan ISO directory", "error", err, "path", d.localISOPath)
		} else {
			result.ISOs = isos
		}
	}
	if result.ISOs == nil {
		result.ISOs = []hostinfo.ISOInfo{}
	}

	return &result, nil
}
