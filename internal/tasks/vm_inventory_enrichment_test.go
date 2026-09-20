package tasks

import (
	"reflect"
	"strings"
	"testing"
)

// Feature: vm-powershell-optimization (task 3.1)
//
// Focused unit tests for parseEnrichmentIntoMaps, the single-parse helper that
// replaces the per-block map population of inventory blocks 2-8. These tests lock
// in the preservation contract for the merged enrichment parse:
//   - array form and single-bare-object form both populate the maps identically
//   - trimPowerShellOutput (BOM + whitespace) is applied before parsing
//   - the len == 0 || "[]" guard short-circuits with no population
//   - NIC values match parseNicsIntoMap's output (id verbatim, Primary=false)
//   - dvdMap only written on non-nil dvd; haMap only written when clustered, by vmId
//
// The fuller assembly + integration coverage is task 3.7 / 3.9; this is the
// foundational helper's own coverage.

// newEnrichmentMaps allocates the full set of enrichment maps the helper populates.
func newEnrichmentMaps() (
	map[string][]VMNic,
	map[string][]VMVHD,
	map[string]*string,
	map[string]bool,
	map[string][]VMSnapshot,
	map[string]vmFirmwareInfo,
	map[string]bool,
) {
	return make(map[string][]VMNic),
		make(map[string][]VMVHD),
		make(map[string]*string),
		make(map[string]bool),
		make(map[string][]VMSnapshot),
		make(map[string]vmFirmwareInfo),
		make(map[string]bool)
}

// A fully-populated combined per-VM object (running, clustered, all attributes).
const enrichmentArrayJSON = `[{"vmName":"web-01","vmId":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","nics":[{"id":"nic-guid-1","name":"Network Adapter","networkId":"vSwitch","macAddress":"00155D010101","connected":true,"ipAddresses":"10.0.0.5, 10.0.0.6","vlanId":100}],"vhds":[{"diskId":"disk-1","path":"C:\\Hyper-V\\web-01.vhdx","format":"VHDX","type":"Dynamic","sizeGb":127.5,"usedBytes":68719476736}],"dvd":"C:\\ISOs\\setup.iso","nested":true,"snapshots":[{"id":"snap-1","name":"before-update","creationTime":"2023-06-16T08:00:00.0000000+00:00","snapshotType":"Standard","parentSnapshotId":null,"parentSnapshotName":null}],"secureBoot":true,"secureBootTemplate":"Windows","ha":true}]`

func TestParseEnrichmentIntoMaps_ArrayForm_ClusteredPopulatesAllMaps(t *testing.T) {
	nicsMap, vhdsMap, dvdMap, nestedMap, snapshotsMap, firmwareMap, haMap := newEnrichmentMaps()

	parseEnrichmentIntoMaps([]byte(enrichmentArrayJSON), true,
		nicsMap, vhdsMap, dvdMap, nestedMap, snapshotsMap, firmwareMap, haMap)

	wantNics := map[string][]VMNic{
		"web-01": {{ID: "nic-guid-1", Name: "Network Adapter", NetworkID: "vSwitch", MacAddress: "00155D010101", Primary: false, Connected: true, IPAddresses: "10.0.0.5, 10.0.0.6", VlanID: 100}},
	}
	if !reflect.DeepEqual(nicsMap, wantNics) {
		t.Fatalf("nicsMap mismatch:\n got:  %#v\n want: %#v", nicsMap, wantNics)
	}

	wantVHDs := map[string][]VMVHD{
		"web-01": {{DiskID: "disk-1", Path: `C:\Hyper-V\web-01.vhdx`, Format: "VHDX", Type: "Dynamic", SizeGB: 127.5, UsedBytes: 68719476736}},
	}
	if !reflect.DeepEqual(vhdsMap, wantVHDs) {
		t.Fatalf("vhdsMap mismatch:\n got:  %#v\n want: %#v", vhdsMap, wantVHDs)
	}

	if dvdMap["web-01"] == nil || *dvdMap["web-01"] != `C:\ISOs\setup.iso` {
		t.Fatalf("dvdMap mismatch: got %#v", dvdMap)
	}

	if !nestedMap["web-01"] {
		t.Fatalf("nestedMap should be true for web-01, got %#v", nestedMap)
	}

	wantSnaps := map[string][]VMSnapshot{
		"web-01": {{ID: "snap-1", Name: "before-update", CreationTime: "2023-06-16T08:00:00.0000000+00:00", SnapshotType: "Standard", ParentSnapshotID: nil, ParentSnapshotName: nil}},
	}
	if !reflect.DeepEqual(snapshotsMap, wantSnaps) {
		t.Fatalf("snapshotsMap mismatch:\n got:  %#v\n want: %#v", snapshotsMap, wantSnaps)
	}

	wantFW := vmFirmwareInfo{VMName: "web-01", SecureBoot: true, SecureBootTemplate: "Windows"}
	if firmwareMap["web-01"] != wantFW {
		t.Fatalf("firmwareMap mismatch: got %#v want %#v", firmwareMap["web-01"], wantFW)
	}

	if !haMap["aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"] {
		t.Fatalf("haMap should be keyed by vmId and true, got %#v", haMap)
	}
}

func TestParseEnrichmentIntoMaps_SingleBareObject_IsWrapped(t *testing.T) {
	// PowerShell emits a bare object (not an array) when only one VM is present.
	single := `{"vmName":"solo","vmId":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","nics":[{"id":"n3","name":"NIC","networkId":"sw","macAddress":"AA","ipAddresses":"1.1.1.1","vlanId":7}],"vhds":[],"dvd":null,"nested":false,"snapshots":[],"secureBoot":false,"secureBootTemplate":"","ha":false}`

	nicsMap, vhdsMap, dvdMap, nestedMap, snapshotsMap, firmwareMap, haMap := newEnrichmentMaps()
	parseEnrichmentIntoMaps([]byte(single), true,
		nicsMap, vhdsMap, dvdMap, nestedMap, snapshotsMap, firmwareMap, haMap)

	wantNics := map[string][]VMNic{
		"solo": {{ID: "n3", Name: "NIC", NetworkID: "sw", MacAddress: "AA", Primary: false, IPAddresses: "1.1.1.1", VlanID: 7}},
	}
	if !reflect.DeepEqual(nicsMap, wantNics) {
		t.Fatalf("nicsMap mismatch:\n got:  %#v\n want: %#v", nicsMap, wantNics)
	}
	if _, ok := dvdMap["solo"]; ok {
		t.Fatalf("dvdMap should not contain solo (dvd was null), got %#v", dvdMap)
	}
	if firmwareMap["solo"] != (vmFirmwareInfo{VMName: "solo"}) {
		t.Fatalf("firmwareMap mismatch: got %#v", firmwareMap["solo"])
	}
	// ha=false but clustered=true -> written as false, keyed by vmId
	if v, ok := haMap["bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"]; !ok || v {
		t.Fatalf("haMap should have vmId=false entry, got %#v", haMap)
	}
}

func TestParseEnrichmentIntoMaps_BOMAndWhitespace(t *testing.T) {
	input := append([]byte{0xef, 0xbb, 0xbf}, []byte("\n  "+enrichmentArrayJSON+"  \r\n")...)

	nicsMap, vhdsMap, dvdMap, nestedMap, snapshotsMap, firmwareMap, haMap := newEnrichmentMaps()
	parseEnrichmentIntoMaps(input, true,
		nicsMap, vhdsMap, dvdMap, nestedMap, snapshotsMap, firmwareMap, haMap)

	if len(nicsMap["web-01"]) != 1 {
		t.Fatalf("BOM/whitespace not handled: nicsMap=%#v", nicsMap)
	}
	if firmwareMap["web-01"].SecureBootTemplate != "Windows" {
		t.Fatalf("BOM/whitespace not handled: firmwareMap=%#v", firmwareMap)
	}
}

func TestParseEnrichmentIntoMaps_EmptyGuard(t *testing.T) {
	for _, input := range []string{"", "[]", "  \r\n[]\t ", "\xef\xbb\xbf[]"} {
		nicsMap, vhdsMap, dvdMap, nestedMap, snapshotsMap, firmwareMap, haMap := newEnrichmentMaps()
		parseEnrichmentIntoMaps([]byte(input), true,
			nicsMap, vhdsMap, dvdMap, nestedMap, snapshotsMap, firmwareMap, haMap)

		if len(nicsMap) != 0 || len(vhdsMap) != 0 || len(dvdMap) != 0 ||
			len(nestedMap) != 0 || len(snapshotsMap) != 0 || len(firmwareMap) != 0 || len(haMap) != 0 {
			t.Fatalf("empty guard failed for input %q: maps not empty", input)
		}
	}
}

func TestParseEnrichmentIntoMaps_StandaloneSkipsHA(t *testing.T) {
	// clustered=false: haMap must stay empty even though the object carries ha=true.
	nicsMap, vhdsMap, dvdMap, nestedMap, snapshotsMap, firmwareMap, haMap := newEnrichmentMaps()
	parseEnrichmentIntoMaps([]byte(enrichmentArrayJSON), false,
		nicsMap, vhdsMap, dvdMap, nestedMap, snapshotsMap, firmwareMap, haMap)

	if len(haMap) != 0 {
		t.Fatalf("standalone host must skip HA, got %#v", haMap)
	}
	// Non-HA enrichment still populated.
	if len(nicsMap["web-01"]) != 1 || !nestedMap["web-01"] {
		t.Fatalf("non-HA enrichment should still populate: nics=%#v nested=%#v", nicsMap, nestedMap)
	}
}

func TestParseEnrichmentIntoMaps_MultipleVMsGroupedByName(t *testing.T) {
	input := `[{"vmName":"a","vmId":"id-a","nics":[{"id":"n1","name":"NIC1","networkId":"s1","macAddress":"AA","ipAddresses":"","vlanId":0}],"vhds":[],"dvd":null,"nested":false,"snapshots":[],"secureBoot":false,"secureBootTemplate":"","ha":false},{"vmName":"b","vmId":"id-b","nics":[{"id":"n2","name":"NIC2","networkId":"s2","macAddress":"BB","ipAddresses":"","vlanId":5}],"vhds":[],"dvd":null,"nested":true,"snapshots":[],"secureBoot":true,"secureBootTemplate":"Linux","ha":true}]`

	nicsMap, vhdsMap, dvdMap, nestedMap, snapshotsMap, firmwareMap, haMap := newEnrichmentMaps()
	parseEnrichmentIntoMaps([]byte(input), true,
		nicsMap, vhdsMap, dvdMap, nestedMap, snapshotsMap, firmwareMap, haMap)

	if len(nicsMap["a"]) != 1 || len(nicsMap["b"]) != 1 {
		t.Fatalf("nics not grouped by vmName: %#v", nicsMap)
	}
	if nestedMap["a"] || !nestedMap["b"] {
		t.Fatalf("nestedMap per-VM mismatch: %#v", nestedMap)
	}
	if firmwareMap["b"].SecureBootTemplate != "Linux" {
		t.Fatalf("firmwareMap per-VM mismatch: %#v", firmwareMap)
	}
	if haMap["id-a"] || !haMap["id-b"] {
		t.Fatalf("haMap keyed by vmId mismatch: %#v", haMap)
	}
	_ = vhdsMap
	_ = dvdMap
	_ = snapshotsMap
}

// Feature: vm-powershell-optimization (task 3.3)
//
// These tests lock in the cluster-cmdlet guard contract for the merged enrichment
// pass. Requirement 3.2 mandates that a non-cluster host invokes NO cluster cmdlet
// at all; the merged script satisfies this by wrapping Get-ClusterResource in an
// `if ($clustered)` block whose flag mirrors d.cluster.IsClusterNode. Because the
// script is host-coupled (it only runs against a live Hyper-V host), we assert its
// structure at the source-string level via the pure buildMergedEnrichmentScript
// builder rather than through a live spawn.

func TestBuildMergedEnrichmentScript_ClusterFlagReflectsIsClusterNode(t *testing.T) {
	if !strings.Contains(buildMergedEnrichmentScript(true), "$clustered = $true") {
		t.Fatalf("clustered=true must set $clustered = $true in the merged script")
	}
	if !strings.Contains(buildMergedEnrichmentScript(false), "$clustered = $false") {
		t.Fatalf("clustered=false must set $clustered = $false in the merged script")
	}
}

func TestBuildMergedEnrichmentScript_ClusterCmdletOnlyInsideGuard(t *testing.T) {
	for _, clustered := range []bool{true, false} {
		script := buildMergedEnrichmentScript(clustered)

		// The cluster cmdlet must appear exactly once and only within the guard.
		if strings.Count(script, "Get-ClusterResource") != 1 {
			t.Fatalf("clustered=%v: expected exactly one Get-ClusterResource occurrence, got %d",
				clustered, strings.Count(script, "Get-ClusterResource"))
		}

		guardIdx := strings.Index(script, "if ($clustered) {")
		if guardIdx == -1 {
			t.Fatalf("clustered=%v: merged script is missing the `if ($clustered)` guard", clustered)
		}

		// Extract the guard block body (from the guard opening brace to its matching
		// close) and assert the cluster cmdlet lives inside it.
		guardBody := clusterGuardBlock(t, script, guardIdx)
		if !strings.Contains(guardBody, "Get-ClusterResource -VMId $v.Id") {
			t.Fatalf("clustered=%v: Get-ClusterResource must be keyed by $v.Id inside the $clustered guard; guard body:\n%s",
				clustered, guardBody)
		}

		// Nothing outside the guard may reference any cluster cmdlet.
		outside := script[:guardIdx] + script[guardIdx+len(guardBody):]
		if strings.Contains(outside, "Get-Cluster") {
			t.Fatalf("clustered=%v: a cluster cmdlet leaked outside the $clustered guard", clustered)
		}
	}
}

// clusterGuardBlock returns the substring of script starting at the `if ($clustered)`
// guard at guardIdx through its matching closing brace, using simple brace counting.
func clusterGuardBlock(t *testing.T, script string, guardIdx int) string {
	t.Helper()
	openIdx := strings.Index(script[guardIdx:], "{")
	if openIdx == -1 {
		t.Fatalf("guard has no opening brace")
	}
	openIdx += guardIdx
	depth := 0
	for i := openIdx; i < len(script); i++ {
		switch script[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return script[guardIdx : i+1]
			}
		}
	}
	t.Fatalf("guard block has no matching closing brace")
	return ""
}
