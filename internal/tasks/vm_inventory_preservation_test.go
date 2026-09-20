package tasks

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

// Feature: vm-powershell-optimization
// Property 2: Preservation - Functional Output Equivalence
//
// **Validates: Requirements 3.1, 3.2, 3.3, 3.4 (inventory side)**
//
// These are PRESERVATION BASELINE tests (golden output). They capture the exact
// output of the CURRENT parse/assembly logic so the later behavior-preserving
// refactor (task 3) can be proven to reproduce it byte-for-byte (re-run in 3.9).
//
// OBSERVATION-FIRST METHODOLOGY:
// The inventory parse/assembly is tightly coupled to hyperv.RunPowerShell (each of
// the 8 blocks spawns a process), so it cannot be driven end-to-end without a live
// Hyper-V host. The pure, host-independent portions of that path are exercised here:
//   - parseNicsIntoMap          : recorded NIC block JSON -> VMNic map
//   - trimPowerShellOutput       : BOM + whitespace normalization on block output
//   - assembleVMInventory        : the final per-VM assembly loop (empty-array-not-null,
//                                  HA-by-vmId, field mapping)
// The remaining block parses (VHD/HA/nested+DVD/snapshot/firmware) are inline in
// handleVMInventory; their DATA consequences are captured by populating the
// enrichment maps and asserting the assembled VMInventoryResult. Full end-to-end
// inventory parity (live-host driven) is exercised by the integration matrix (task 5).
//
// EXPECTED OUTCOME ON UNFIXED CODE: these tests PASS (they define the baseline).

// ---------------------------------------------------------------------------
// 2a / 2b: full-inventory golden snapshots (marshalled VMInventoryResult JSON)
// ---------------------------------------------------------------------------

// richBasic is a fully-populated running VM used as the "cluster + running + all
// attributes present" golden fixture.
func richBasic() vmBasicInfo {
	return vmBasicInfo{
		ID:                  "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		Name:                "web-01",
		PowerState:          "Running",
		CPUCount:            4,
		MemoryMB:            8192,
		MemoryMinBytes:      2147483648,
		MemoryMaxBytes:      17179869184,
		State:               "Running",
		Notes:               "prod web",
		UptimeSeconds:       3600,
		Uptime:              "01:00:00",
		CPUUsagePercent:     5,
		MemoryConsumedMB:    4096,
		AutomaticStart:      "Start",
		AutomaticStartDelay: 10,
		AutomaticStop:       "ShutDown",
		Path:                `C:\ClusterStorage\Volume1\web-01`,
		CreationTime:        "2023-06-15T12:30:00.0000000+00:00",
	}
}

// bareBasic is a minimal off VM with no enrichment (used for empty-array-not-null
// and standalone-host golden fixtures).
func bareBasic() vmBasicInfo {
	return vmBasicInfo{
		ID:                  "11111111-1111-1111-1111-111111111111",
		Name:                "vm1",
		PowerState:          "Off",
		CPUCount:            2,
		MemoryMB:            2048,
		State:               "Off",
		Notes:               "",
		UptimeSeconds:       0,
		Uptime:              "00:00:00",
		CPUUsagePercent:     0,
		MemoryConsumedMB:    0,
		AutomaticStart:      "StartIfRunning",
		AutomaticStartDelay: 0,
		AutomaticStop:       "Save",
		Path:                `C:\VMs\vm1`,
		CreationTime:        "2024-01-01T00:00:00.0000000+00:00",
	}
}

func TestPreservation_AssembleVMInventory_Golden(t *testing.T) {
	tests := []struct {
		name       string
		basics     []vmBasicInfo
		nicsMap    map[string][]VMNic
		vhdsMap    map[string][]VMVHD
		dvdMap     map[string]*string
		nestedMap  map[string]bool
		snapshots  map[string][]VMSnapshot
		firmware   map[string]vmFirmwareInfo
		haMap      map[string]bool
		goldenJSON string
	}{
		{
			name:       "0 VMs yields empty vms array",
			basics:     []vmBasicInfo{},
			goldenJSON: `{"vms":[]}`,
		},
		{
			name:   "bare VM on standalone host: empty arrays not null, ha false, dvd null",
			basics: []vmBasicInfo{bareBasic()},
			// standalone host -> haMap empty; no enrichment present.
			goldenJSON: `{"vms":[{"id":"11111111-1111-1111-1111-111111111111","name":"vm1","powerState":"Off","cpu":{"vcpus":2},"memoryMb":2048,"memoryMinBytes":0,"memoryMaxBytes":0,"state":"Off","notes":"","uptimeSec":0,"uptime":"00:00:00","cpuUsagePct":0,"memoryConsumedMb":0,"automaticStart":"StartIfRunning","automaticStartDelay":0,"automaticStop":"Save","path":"C:\\VMs\\vm1","creationTime":"2024-01-01T00:00:00.0000000+00:00","nics":[],"vhds":[],"dvd":null,"snapshots":[],"ha":false,"nestedVirtualization":false,"secureBoot":false,"secureBootTemplate":"","metricsEnabled":false}]}`,
		},
		{
			name:   "rich running VM on cluster: all attributes, ha true",
			basics: []vmBasicInfo{richBasic()},
			nicsMap: map[string][]VMNic{
				"web-01": {{ID: "nic-guid-1", Name: "Network Adapter", NetworkID: "vSwitch", MacAddress: "00155D010101", Primary: false, IPAddresses: "10.0.0.5, 10.0.0.6", VlanID: 100}},
			},
			vhdsMap: map[string][]VMVHD{
				"web-01": {{DiskID: "disk-1", Path: `C:\Hyper-V\web-01.vhdx`, Format: "VHDX", Type: "Dynamic", SizeGB: 127.5, UsedBytes: 68719476736}},
			},
			dvdMap:    map[string]*string{"web-01": strPtr(`C:\ISOs\setup.iso`)},
			nestedMap: map[string]bool{"web-01": true},
			snapshots: map[string][]VMSnapshot{
				"web-01": {{ID: "snap-1", Name: "before-update", CreationTime: "2023-06-16T08:00:00.0000000+00:00", SnapshotType: "Standard", ParentSnapshotID: nil, ParentSnapshotName: nil}},
			},
			firmware: map[string]vmFirmwareInfo{
				"web-01": {VMName: "web-01", SecureBoot: true, SecureBootTemplate: "Windows"},
			},
			haMap:      map[string]bool{"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa": true},
			goldenJSON: `{"vms":[{"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","name":"web-01","powerState":"Running","cpu":{"vcpus":4},"memoryMb":8192,"memoryMinBytes":2147483648,"memoryMaxBytes":17179869184,"state":"Running","notes":"prod web","uptimeSec":3600,"uptime":"01:00:00","cpuUsagePct":5,"memoryConsumedMb":4096,"automaticStart":"Start","automaticStartDelay":10,"automaticStop":"ShutDown","path":"C:\\ClusterStorage\\Volume1\\web-01","creationTime":"2023-06-15T12:30:00.0000000+00:00","nics":[{"id":"nic-guid-1","name":"Network Adapter","networkId":"vSwitch","macAddress":"00155D010101","primary":false,"connected":false,"ipAddresses":"10.0.0.5, 10.0.0.6","vlanId":100}],"vhds":[{"diskId":"disk-1","path":"C:\\Hyper-V\\web-01.vhdx","format":"VHDX","type":"Dynamic","sizeGb":127.5,"usedBytes":68719476736}],"dvd":"C:\\ISOs\\setup.iso","snapshots":[{"id":"snap-1","name":"before-update","creationTime":"2023-06-16T08:00:00.0000000+00:00","snapshotType":"Standard","parentSnapshotId":null,"parentSnapshotName":null}],"ha":true,"nestedVirtualization":true,"secureBoot":true,"secureBootTemplate":"Windows","metricsEnabled":false}]}`,
		},
		{
			name:   "many VMs: input order preserved, mixed running/off, cluster",
			basics: []vmBasicInfo{richBasic(), bareBasic()},
			nicsMap: map[string][]VMNic{
				"web-01": {{ID: "nic-guid-1", Name: "Network Adapter", NetworkID: "vSwitch", MacAddress: "00155D010101", Primary: false, IPAddresses: "10.0.0.5", VlanID: 0}},
			},
			// only vm1 (the off VM) has HA in the cluster map; web-01 does not.
			haMap:      map[string]bool{"11111111-1111-1111-1111-111111111111": true},
			goldenJSON: `{"vms":[{"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","name":"web-01","powerState":"Running","cpu":{"vcpus":4},"memoryMb":8192,"memoryMinBytes":2147483648,"memoryMaxBytes":17179869184,"state":"Running","notes":"prod web","uptimeSec":3600,"uptime":"01:00:00","cpuUsagePct":5,"memoryConsumedMb":4096,"automaticStart":"Start","automaticStartDelay":10,"automaticStop":"ShutDown","path":"C:\\ClusterStorage\\Volume1\\web-01","creationTime":"2023-06-15T12:30:00.0000000+00:00","nics":[{"id":"nic-guid-1","name":"Network Adapter","networkId":"vSwitch","macAddress":"00155D010101","primary":false,"connected":false,"ipAddresses":"10.0.0.5","vlanId":0}],"vhds":[],"dvd":null,"snapshots":[],"ha":false,"nestedVirtualization":false,"secureBoot":false,"secureBootTemplate":"","metricsEnabled":false},{"id":"11111111-1111-1111-1111-111111111111","name":"vm1","powerState":"Off","cpu":{"vcpus":2},"memoryMb":2048,"memoryMinBytes":0,"memoryMaxBytes":0,"state":"Off","notes":"","uptimeSec":0,"uptime":"00:00:00","cpuUsagePct":0,"memoryConsumedMb":0,"automaticStart":"StartIfRunning","automaticStartDelay":0,"automaticStop":"Save","path":"C:\\VMs\\vm1","creationTime":"2024-01-01T00:00:00.0000000+00:00","nics":[],"vhds":[],"dvd":null,"snapshots":[],"ha":true,"nestedVirtualization":false,"secureBoot":false,"secureBootTemplate":"","metricsEnabled":false}]}`,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			vms := assembleVMInventory(tc.basics, tc.nicsMap, tc.vhdsMap, tc.dvdMap, tc.nestedMap, tc.snapshots, tc.firmware, tc.haMap)
			got, err := json.Marshal(&VMInventoryResult{VMs: vms})
			if err != nil {
				t.Fatalf("marshal failed: %v", err)
			}
			if string(got) != tc.goldenJSON {
				t.Fatalf("golden mismatch:\n got:  %s\n want: %s", string(got), tc.goldenJSON)
			}
		})
	}
}

// 2d (data consequence): when an enrichment block fails, handleVMInventory logs a
// warning and continues with that block's map empty. The assembled inventory must
// therefore be basic-info-only with empty arrays / false flags. nil maps model the
// "block never populated its map" case and must be handled safely.
func TestPreservation_AssembleVMInventory_EnrichmentFailure_BasicOnly(t *testing.T) {
	vms := assembleVMInventory([]vmBasicInfo{richBasic()}, nil, nil, nil, nil, nil, nil, nil)
	got, err := json.Marshal(&VMInventoryResult{VMs: vms})
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	want := `{"vms":[{"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","name":"web-01","powerState":"Running","cpu":{"vcpus":4},"memoryMb":8192,"memoryMinBytes":2147483648,"memoryMaxBytes":17179869184,"state":"Running","notes":"prod web","uptimeSec":3600,"uptime":"01:00:00","cpuUsagePct":5,"memoryConsumedMb":4096,"automaticStart":"Start","automaticStartDelay":10,"automaticStop":"ShutDown","path":"C:\\ClusterStorage\\Volume1\\web-01","creationTime":"2023-06-15T12:30:00.0000000+00:00","nics":[],"vhds":[],"dvd":null,"snapshots":[],"ha":false,"nestedVirtualization":false,"secureBoot":false,"secureBootTemplate":"","metricsEnabled":false}]}`
	if string(got) != want {
		t.Fatalf("graceful-degradation baseline mismatch:\n got:  %s\n want: %s", string(got), want)
	}
}

// ---------------------------------------------------------------------------
// 2a: NIC block parse golden (real parseNicsIntoMap)
// ---------------------------------------------------------------------------

func TestPreservation_ParseNicsIntoMap_Golden(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  map[string][]VMNic
	}{
		{
			name:  "empty bracket yields no entries",
			input: `[]`,
			want:  map[string][]VMNic{},
		},
		{
			name:  "empty output yields no entries",
			input: ``,
			want:  map[string][]VMNic{},
		},
		{
			name:  "running VM: IPs and VLAN preserved",
			input: `[{"vmName":"web-01","id":"nic-1","name":"Network Adapter","networkId":"vSwitch","macAddress":"00155D010101","ipAddresses":"10.0.0.5, 10.0.0.6","vlanId":100}]`,
			want: map[string][]VMNic{
				"web-01": {{ID: "nic-1", Name: "Network Adapter", NetworkID: "vSwitch", MacAddress: "00155D010101", Primary: false, IPAddresses: "10.0.0.5, 10.0.0.6", VlanID: 100}},
			},
		},
		{
			name:  "off VM: empty IPs preserved",
			input: `[{"vmName":"vm1","id":"nic-2","name":"Network Adapter","networkId":"vSwitch","macAddress":"00155D010102","ipAddresses":"","vlanId":0}]`,
			want: map[string][]VMNic{
				"vm1": {{ID: "nic-2", Name: "Network Adapter", NetworkID: "vSwitch", MacAddress: "00155D010102", Primary: false, IPAddresses: "", VlanID: 0}},
			},
		},
		{
			name:  "single bare object (not array) is wrapped",
			input: `{"vmName":"solo","id":"nic-3","name":"NIC","networkId":"sw","macAddress":"AA","ipAddresses":"1.1.1.1","vlanId":7}`,
			want: map[string][]VMNic{
				"solo": {{ID: "nic-3", Name: "NIC", NetworkID: "sw", MacAddress: "AA", Primary: false, IPAddresses: "1.1.1.1", VlanID: 7}},
			},
		},
		{
			name:  "multiple NICs across VMs grouped by vmName",
			input: `[{"vmName":"a","id":"n1","name":"NIC1","networkId":"s1","macAddress":"AA","ipAddresses":"","vlanId":0},{"vmName":"a","id":"n2","name":"NIC2","networkId":"s2","macAddress":"BB","ipAddresses":"","vlanId":0},{"vmName":"b","id":"n3","name":"NIC3","networkId":"s3","macAddress":"CC","ipAddresses":"","vlanId":5}]`,
			want: map[string][]VMNic{
				"a": {
					{ID: "n1", Name: "NIC1", NetworkID: "s1", MacAddress: "AA", IPAddresses: "", VlanID: 0},
					{ID: "n2", Name: "NIC2", NetworkID: "s2", MacAddress: "BB", IPAddresses: "", VlanID: 0},
				},
				"b": {{ID: "n3", Name: "NIC3", NetworkID: "s3", MacAddress: "CC", IPAddresses: "", VlanID: 5}},
			},
		},
		{
			name:  "UTF-8 BOM prefixed output parsed correctly",
			input: "\xef\xbb\xbf" + `[{"vmName":"bom","id":"n1","name":"NIC","networkId":"s","macAddress":"AA","ipAddresses":"","vlanId":0}]`,
			want: map[string][]VMNic{
				"bom": {{ID: "n1", Name: "NIC", NetworkID: "s", MacAddress: "AA", IPAddresses: "", VlanID: 0}},
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := make(map[string][]VMNic)
			parseNicsIntoMap([]byte(tc.input), got)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseNicsIntoMap mismatch:\n got:  %#v\n want: %#v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 2a: BOM / whitespace normalization golden (real trimPowerShellOutput)
// ---------------------------------------------------------------------------

func TestPreservation_TrimPowerShellOutput_Golden(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  string
	}{
		{"strips UTF-8 BOM", append([]byte{0xef, 0xbb, 0xbf}, []byte("[]")...), "[]"},
		{"trims leading and trailing whitespace", []byte("  \r\n[]\t\n "), "[]"},
		{"BOM plus surrounding whitespace", append([]byte{0xef, 0xbb, 0xbf}, []byte("\n  {\"a\":1}  \r\n")...), `{"a":1}`},
		{"no BOM no whitespace unchanged", []byte(`{"x":true}`), `{"x":true}`},
		{"all whitespace collapses to empty", []byte("  \r\n\t "), ""},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := string(trimPowerShellOutput(tc.input)); got != tc.want {
				t.Fatalf("trimPowerShellOutput = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 2b / 2c: universal invariants over random inventories (testing/quick)
// ---------------------------------------------------------------------------

// TestPreservation_AssembleVMInventory_Invariants asserts the preservation
// invariants hold for arbitrary basic-info sets and enrichment maps:
//   - one VMInfo per basic entry, in input order (id preserved)
//   - nics/vhds/snapshots are never null (always at least an empty slice)
//   - ha is exactly haMap[id] (empty/standalone map => ha=false; cluster-skip)
func TestPreservation_AssembleVMInventory_Invariants(t *testing.T) {
	cfg := &quick.Config{MaxCount: 300}
	err := quick.Check(func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		n := r.Intn(6) // 0..5 VMs
		basics := make([]vmBasicInfo, n)
		haMap := make(map[string]bool)
		nicsMap := make(map[string][]VMNic)
		for i := range basics {
			id := randID(r)
			name := randName(r)
			basics[i] = vmBasicInfo{ID: id, Name: name, State: pickState(r)}
			// randomly assign HA and NICs
			if r.Intn(2) == 0 {
				haMap[id] = true
			}
			if r.Intn(2) == 0 {
				nicsMap[name] = []VMNic{{ID: "n", Name: "NIC"}}
			}
		}

		vms := assembleVMInventory(basics, nicsMap, nil, nil, nil, nil, nil, haMap)

		if len(vms) != len(basics) {
			return false
		}
		for i, vm := range vms {
			if vm.ID != basics[i].ID || vm.Name != basics[i].Name {
				return false // order / identity preserved
			}
			if vm.NICs == nil || vm.VHDs == nil || vm.Snapshots == nil {
				return false // empty-array-not-null
			}
			if vm.HA != haMap[basics[i].ID] {
				return false // HA keyed by vmId; standalone (absent) => false
			}
		}
		return true
	}, cfg)
	if err != nil {
		t.Fatalf("preservation invariant violated: %v", err)
	}
}

// TestPreservation_AssembleVMInventory_EmptyArrayNotNull_JSON asserts at the JSON
// byte level that nics/vhds/snapshots serialize as [] (never null) for a VM with
// no enrichment, while dvd (a nullable pointer) serializes as null.
func TestPreservation_AssembleVMInventory_EmptyArrayNotNull_JSON(t *testing.T) {
	vms := assembleVMInventory([]vmBasicInfo{bareBasic()}, nil, nil, nil, nil, nil, nil, nil)
	b, err := json.Marshal(&VMInventoryResult{VMs: vms})
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	out := string(b)
	for _, want := range []string{`"nics":[]`, `"vhds":[]`, `"snapshots":[]`, `"dvd":null`, `"ha":false`} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %s in output, got: %s", want, out)
		}
	}
	for _, bad := range []string{`"nics":null`, `"vhds":null`, `"snapshots":null`} {
		if strings.Contains(out, bad) {
			t.Fatalf("did not expect %s in output, got: %s", bad, out)
		}
	}
}

// --- small test helpers ---

func strPtr(s string) *string { return &s }

func randID(r *rand.Rand) string {
	const hex = "0123456789abcdef"
	b := make([]byte, 36)
	for i := range b {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			b[i] = '-'
			continue
		}
		b[i] = hex[r.Intn(16)]
	}
	return string(b)
}

func randName(r *rand.Rand) string {
	names := []string{"vm-a", "vm-b", "vm-c", "vm-d", "srv1", "srv2"}
	return names[r.Intn(len(names))]
}

func pickState(r *rand.Rand) string {
	states := []string{"Running", "Off", "Saved", "Paused"}
	return states[r.Intn(len(states))]
}
