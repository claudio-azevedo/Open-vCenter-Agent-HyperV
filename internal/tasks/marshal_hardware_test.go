package tasks

import (
	"encoding/json"
	"testing"
)

func TestHardwareInventoryResult_MarshalsCanonicalShape(t *testing.T) {
	r := &HardwareInventoryResult{
		OSName:             "Microsoft Windows Server 2022 Standard Evaluation",
		OSVersion:          "10.0.20348",
		LogicalProcessors:  4,
		CPUSockets:         1,
		CPUCores:           2,
		MemoryCapacityMB:   8066.15,
		HWManufacturer:     "TOSHIBA",
		HWModel:            "PORTEGE R930",
		CPUModel:           "Intel(R) Core(TM) i5-3320M CPU @ 2.60GHz",
		CPULoadPercent:     75,
		MemoryUsagePercent: 34.15,
		LastBootTime:       "2026-08-29T18:32:57.1791582-03:00",
		DefaultVMPath:      `D:\HyperV\VMS\`,
		DefaultVHDPath:     `D:\HyperV\VMS\`,
		IsCluster:          false,
		ClusterState:       "Not Clustered",
		DiskC:              DiskInfo{TotalGB: 62.07, FreeGB: 42.51},
		Storage:            []StorageInfo{{Name: "VMs", Path: `D:\`, TotalGB: 175.75, FreeGB: 173.42}},
		NetAdapters: []NetAdapterInfo{
			{Name: "Ethernet", Description: "Realtek PCIe GbE", MAC: "00-11-22-33-44-55", SpeedBps: 1_000_000_000, Connected: true},
			{Name: "Ethernet 2", Description: "Intel I350", MAC: "00-11-22-33-44-56", SpeedBps: 0, Connected: false},
		},
		VSwitches: []VSwitchInfo{
			{Name: "vSwitch-Prod", Type: "External", NetAdapter: "Realtek PCIe GbE"},
		},
	}

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	cpu := got["cpu"].(map[string]any)
	if cpu["model"] != r.CPUModel || cpu["sockets"].(float64) != 1 ||
		cpu["cores"].(float64) != 2 || cpu["logical"].(float64) != 4 {
		t.Errorf("cpu = %v", cpu)
	}
	if got["memoryBytes"].(float64) != 8457971302 { // 8066.15 * 1024^2
		t.Errorf("memoryBytes = %v", got["memoryBytes"])
	}
	storage := got["storage"].([]any)
	if len(storage) != 2 || storage[0].(map[string]any)["path"] != `C:\` {
		t.Errorf("storage = %v", storage)
	}
	if d := storage[1].(map[string]any); d["label"] != "VMs" || d["path"] != `D:\` {
		t.Errorf("storage[1] = %v", d)
	}
	network, ok := got["network"].([]any)
	if !ok || len(network) != 2 {
		t.Fatalf("network must be a 2-element array, got %T %v", got["network"], got["network"])
	}
	if n0 := network[0].(map[string]any); n0["name"] != "Ethernet" || n0["connected"] != true ||
		n0["description"] != "Realtek PCIe GbE" || n0["speedBps"].(float64) != 1e9 {
		t.Errorf("network[0] = %v", n0)
	}
	if n1 := network[1].(map[string]any); n1["connected"] != false {
		t.Errorf("network[1].connected should be false, got %v", n1["connected"])
	}
	vsw, ok := got["vSwitches"].([]any)
	if !ok || len(vsw) != 1 {
		t.Fatalf("vSwitches must be a 1-element array, got %T %v", got["vSwitches"], got["vSwitches"])
	}
	if s0 := vsw[0].(map[string]any); s0["name"] != "vSwitch-Prod" || s0["type"] != "External" ||
		s0["netAdapter"] != "Realtek PCIe GbE" {
		t.Errorf("vSwitches[0] = %v", s0)
	}
	if sys := got["system"].(map[string]any); sys["manufacturer"] != "TOSHIBA" || sys["model"] != "PORTEGE R930" {
		t.Errorf("system = %v", sys)
	}
	if got["bootTime"] != r.LastBootTime {
		t.Errorf("bootTime = %v", got["bootTime"])
	}
	if load := got["load"].(map[string]any); load["cpuPercent"].(float64) != 75 {
		t.Errorf("load = %v", load)
	}
	if cl := got["cluster"].(map[string]any); cl["clustered"] != false || cl["state"] != "Not Clustered" {
		t.Errorf("cluster = %v", cl)
	}
	if hv := got["hyperv"].(map[string]any); hv["defaultVmPath"] != `D:\HyperV\VMS\` {
		t.Errorf("hyperv = %v", hv)
	}

	// keys that must NOT leak from the raw shape
	for _, k := range []string{"cpuModel", "memoryCapacityMB", "diskC", "hwModel", "isCluster", "isos", "netAdapters"} {
		if _, present := got[k]; present {
			t.Errorf("raw key %q leaked into canonical output", k)
		}
	}
}
