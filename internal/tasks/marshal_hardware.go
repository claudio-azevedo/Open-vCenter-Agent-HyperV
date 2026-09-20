package tasks

import (
	"encoding/json"
	"math"
)

// The wire shape of a host hardware inventory. Matches ovc-backend's
// HostHardwareInventory (schemas/host.py) - camelCase, bytes not GB, one
// storage[] list, plus the extended blocks (system / boot / load / cluster /
// hyperv) the backend now carries. `network` is always [] for the Hyper-V agent.
const (
	bytesPerGiB = 1024 * 1024 * 1024
	bytesPerMiB = 1024 * 1024
)

type hwCPU struct {
	Model   string `json:"model"`
	Sockets int    `json:"sockets"`
	Cores   int    `json:"cores"`
	Logical int    `json:"logical"`
}

type hwVolume struct {
	Path       string `json:"path"`
	Label      string `json:"label,omitempty"`
	TotalBytes int64  `json:"totalBytes"`
	FreeBytes  int64  `json:"freeBytes"`
}

type hwOS struct {
	Caption string `json:"caption"`
	Version string `json:"version"`
}

type hwSystem struct {
	Manufacturer string `json:"manufacturer"`
	Model        string `json:"model"`
}

type hwLoad struct {
	CPUPercent    float64 `json:"cpuPercent"`
	MemoryPercent float64 `json:"memoryPercent"`
}

type hwCluster struct {
	Clustered bool     `json:"clustered"`
	Name      string   `json:"name,omitempty"`
	State     string   `json:"state"`
	Nodes     []string `json:"nodes"`
}

type hwHyperV struct {
	DefaultVMPath  string `json:"defaultVmPath"`
	DefaultVHDPath string `json:"defaultVhdPath"`
}

type hwNIC struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	MAC         string `json:"mac"`
	SpeedBps    int64  `json:"speedBps"`
	Connected   bool   `json:"connected"`
}

type hwVSwitch struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	NetAdapter string `json:"netAdapter,omitempty"`
}

type hwWire struct {
	CPU         hwCPU       `json:"cpu"`
	MemoryBytes int64       `json:"memoryBytes"`
	Storage     []hwVolume  `json:"storage"`
	OS          hwOS        `json:"os"`
	Network     []hwNIC     `json:"network"`
	VSwitches   []hwVSwitch `json:"vSwitches"`
	System      *hwSystem   `json:"system"`
	BootTime    string      `json:"bootTime,omitempty"`
	Load        *hwLoad     `json:"load"`
	Cluster     *hwCluster  `json:"cluster"`
	HyperV      *hwHyperV   `json:"hyperv"`
}

func gibToBytes(gb float64) int64 { return int64(math.Round(gb * bytesPerGiB)) }

// MarshalJSON emits the canonical HostHardwareInventory shape.
func (r *HardwareInventoryResult) MarshalJSON() ([]byte, error) {
	storage := make([]hwVolume, 0, len(r.Storage)+1)
	if r.DiskC.TotalGB > 0 {
		storage = append(storage, hwVolume{
			Path:       `C:\`,
			TotalBytes: gibToBytes(r.DiskC.TotalGB),
			FreeBytes:  gibToBytes(r.DiskC.FreeGB),
		})
	}
	for _, s := range r.Storage {
		storage = append(storage, hwVolume{
			Path:       s.Path,
			Label:      s.Name,
			TotalBytes: gibToBytes(s.TotalGB),
			FreeBytes:  gibToBytes(s.FreeGB),
		})
	}

	logical := r.LogicalProcessors
	cores := r.CPUCores
	if cores == 0 {
		cores = logical
	}
	sockets := r.CPUSockets
	if sockets == 0 && logical > 0 {
		sockets = 1
	}

	network := make([]hwNIC, 0, len(r.NetAdapters))
	for _, n := range r.NetAdapters {
		network = append(network, hwNIC{
			Name:        n.Name,
			Description: n.Description,
			MAC:         n.MAC,
			SpeedBps:    n.SpeedBps,
			Connected:   n.Connected,
		})
	}
	vSwitches := make([]hwVSwitch, 0, len(r.VSwitches))
	for _, s := range r.VSwitches {
		vSwitches = append(vSwitches, hwVSwitch{Name: s.Name, Type: s.Type, NetAdapter: s.NetAdapter})
	}

	w := hwWire{
		CPU:         hwCPU{Model: r.CPUModel, Sockets: sockets, Cores: cores, Logical: logical},
		MemoryBytes: int64(math.Round(r.MemoryCapacityMB * bytesPerMiB)),
		Storage:     storage,
		OS:          hwOS{Caption: r.OSName, Version: r.OSVersion},
		Network:     network,
		VSwitches:   vSwitches,
		BootTime:    r.LastBootTime,
		Load:        &hwLoad{CPUPercent: r.CPULoadPercent, MemoryPercent: r.MemoryUsagePercent},
	}

	if r.HWManufacturer != "" || r.HWModel != "" {
		w.System = &hwSystem{Manufacturer: r.HWManufacturer, Model: r.HWModel}
	}

	nodes := r.ClusterNodes
	if nodes == nil {
		nodes = []string{}
	}
	w.Cluster = &hwCluster{
		Clustered: r.IsCluster,
		Name:      r.ClusterName,
		State:     r.ClusterState,
		Nodes:     nodes,
	}

	if r.DefaultVMPath != "" || r.DefaultVHDPath != "" {
		w.HyperV = &hwHyperV{DefaultVMPath: r.DefaultVMPath, DefaultVHDPath: r.DefaultVHDPath}
	}

	return json.Marshal(w)
}
