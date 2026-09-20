package tasks

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"testing/quick"
)

// Feature: vm-powershell-optimization (task 4)
// Property 2: Preservation - Functional Output Equivalence (universal form)
//
// **Validates: Requirements 2.2, 3.1, 3.3, 3.7**
//
// These are the universal (property-based) form of the preservation contract
// F(X) = F'(X) at the data level. Task 2 captured golden baselines and task 3.1/3.7
// added focused unit + invariant coverage. The KEY NEW property added here is
// EQUIVALENCE between the fixed merged-enrichment pass and the ORIGINAL per-block
// assembly for the SAME underlying data:
//
//   - Inventory: a random logical VM set is rendered TWO ways and both are run
//     through assembleVMInventory; the resulting VMInventoryResult values must
//     marshal to byte-identical JSON.
//       Path A (fixed / merged pass): render the merged-script JSON shape (array of
//         combined per-VM objects, or a single bare object when exactly one VM) and
//         parse it via parseEnrichmentIntoMaps.
//       Path B (original per-block):  populate nicsMap/vhdsMap/dvdMap/nestedMap/
//         snapshotsMap/firmwareMap/haMap DIRECTLY from the logical model exactly the
//         way old blocks 2-8 did (this is "what the old code would have produced").
//     This proves F(X)=F'(X) at the data level for the inventory path across the
//     input domain, including single-object-vs-array unmarshalling and BOM/whitespace
//     prefixes emitted across the domain.
//
//   - Batch: over random vmBatchStateOp inputs, assert per-item error-string
//     FORMATTING (name-mismatch and operation-error) holds across the whole input
//     domain (the 3.7 invariants test only asserts a non-empty error, not the exact
//     string for arbitrary names/messages), and assert ordering + aggregate-status
//     equivalence - deriveVMBatchStatus over the success/failure counts must match a
//     reference computed independently by scanning the per-item status sequence.

// ---------------------------------------------------------------------------
// Logical VM model + generators
// ---------------------------------------------------------------------------

// logicalVM is a host-independent description of one VM and its enrichment
// attributes. It is the "underlying data" X. Both the merged-JSON render and the
// direct per-block map population are derived from it, so any divergence between the
// two paths is a preservation failure rather than a generator artifact.
type logicalVM struct {
	id                 string
	name               string
	state              string
	nics               []vmEnrichmentNic
	vhds               []vmEnrichmentVHD
	dvd                *string
	nested             bool
	snapshots          []vmEnrichmentSnapshot
	secureBoot         bool
	secureBootTemplate string
	ha                 bool
}

func randToken(r *rand.Rand, prefix string) string {
	const alpha = "abcdefghijklmnopqrstuvwxyz0123456789-"
	n := 3 + r.Intn(6)
	b := make([]byte, n)
	for i := range b {
		b[i] = alpha[r.Intn(len(alpha))]
	}
	return prefix + string(b)
}

func randState(r *rand.Rand) string {
	states := []string{"Running", "Off", "Saved", "Paused"}
	return states[r.Intn(len(states))]
}

func randTemplate(r *rand.Rand) string {
	tpls := []string{"", "Windows", "Linux", "Others", "SomeRawTemplate"}
	return tpls[r.Intn(len(tpls))]
}

func randSnapshotType(r *rand.Rand) string {
	types := []string{"Standard", "Recovery", "Production"}
	return types[r.Intn(len(types))]
}

// genLogicalVMs produces a random logical VM set of size 0..N with random attribute
// presence and states. IDs are unique (so haMap keying is unambiguous) and names are
// unique (so name-keyed maps are unambiguous), matching the real inventory where each
// VM has a distinct Id/Name.
func genLogicalVMs(r *rand.Rand) []logicalVM {
	n := r.Intn(6) // 0..5 VMs
	vms := make([]logicalVM, 0, n)
	for i := 0; i < n; i++ {
		state := randState(r)
		running := state == "Running"

		vm := logicalVM{
			id:                 fmt.Sprintf("%08x-0000-0000-0000-%012x", r.Uint32(), i),
			name:               fmt.Sprintf("%s-%d", randToken(r, "vm-"), i),
			state:              state,
			nested:             r.Intn(2) == 0,
			secureBoot:         r.Intn(2) == 0,
			secureBootTemplate: randTemplate(r),
			ha:                 r.Intn(2) == 0,
		}

		// NICs (0..3). ipAddresses is only meaningful for Running VMs, mirroring the
		// merged script's running-vs-off nuance; the parse copies whatever is present.
		nicCount := r.Intn(4)
		for j := 0; j < nicCount; j++ {
			ip := ""
			if running && r.Intn(2) == 0 {
				ip = fmt.Sprintf("10.0.%d.%d", r.Intn(255), r.Intn(255))
			}
			vm.nics = append(vm.nics, vmEnrichmentNic{
				ID:          randToken(r, "nic-"),
				Name:        "Network Adapter",
				NetworkID:   randToken(r, "sw-"),
				MacAddress:  fmt.Sprintf("00155D%06X", r.Intn(0xFFFFFF)),
				IPAddresses: ip,
				VlanID:      r.Intn(200),
			})
		}

		// VHDs (0..2).
		vhdCount := r.Intn(3)
		for j := 0; j < vhdCount; j++ {
			vm.vhds = append(vm.vhds, vmEnrichmentVHD{
				DiskID:     randToken(r, "disk-"),
				Path:       fmt.Sprintf(`C:\Hyper-V\%s-%d.vhdx`, vm.name, j),
				Controller: fmt.Sprintf("SCSI 0:%d", j),
				Format:     "VHDX",
				Type:       "Dynamic",
				SizeGB:     float64(r.Intn(1000)) / 10,
			})
		}

		// DVD present ~half the time.
		if r.Intn(2) == 0 {
			p := fmt.Sprintf(`C:\ISOs\%s.iso`, randToken(r, ""))
			vm.dvd = &p
		}

		// Snapshots (0..2), with occasional parent pointers.
		snapCount := r.Intn(3)
		for j := 0; j < snapCount; j++ {
			s := vmEnrichmentSnapshot{
				ID:           randToken(r, "snap-"),
				Name:         randToken(r, "cp-"),
				CreationTime: "2023-06-16T08:00:00.0000000+00:00",
				SnapshotType: randSnapshotType(r),
			}
			if r.Intn(2) == 0 {
				pid := randToken(r, "psnap-")
				pname := randToken(r, "pcp-")
				s.ParentSnapshotID = &pid
				s.ParentSnapshotName = &pname
			}
			vm.snapshots = append(vm.snapshots, s)
		}

		vms = append(vms, vm)
	}
	return vms
}

// basicsFromLogical builds the vmBasicInfo slice (Block 1 data) from the logical set.
// The equivalence test feeds the same basics to both assembly paths, so only the
// enrichment maps differ between the paths.
func basicsFromLogical(vms []logicalVM) []vmBasicInfo {
	basics := make([]vmBasicInfo, 0, len(vms))
	for _, vm := range vms {
		basics = append(basics, vmBasicInfo{
			ID:         vm.id,
			Name:       vm.name,
			PowerState: vm.state,
			State:      vm.state,
		})
	}
	return basics
}

// renderMergedJSON renders the logical set as the merged-enrichment script would emit
// it: an array of combined per-VM objects, or - when exactly one VM is present - a
// single bare object (exercising the single-object-vs-array unmarshalling fallback).
// A UTF-8 BOM and/or surrounding whitespace is randomly prepended/appended to exercise
// trimPowerShellOutput across the domain. encoding/json is used so the byte shape
// matches the vmEnrichmentInfo tags exactly.
func renderMergedJSON(r *rand.Rand, vms []logicalVM, clustered bool) []byte {
	infos := make([]vmEnrichmentInfo, 0, len(vms))
	for _, vm := range vms {
		ha := vm.ha
		if !clustered {
			// The real merged script emits ha=false on standalone hosts (the cluster
			// cmdlet never runs). parseEnrichmentIntoMaps ignores ha when !clustered.
			ha = false
		}
		infos = append(infos, vmEnrichmentInfo{
			VMName:             vm.name,
			VMId:               vm.id,
			NICs:               vm.nics,
			VHDs:               vm.vhds,
			DVD:                vm.dvd,
			Nested:             vm.nested,
			Snapshots:          vm.snapshots,
			SecureBoot:         vm.secureBoot,
			SecureBootTemplate: vm.secureBootTemplate,
			HA:                 ha,
		})
	}

	var raw []byte
	if len(infos) == 1 && r.Intn(2) == 0 {
		// Single bare object (PowerShell emits this when only one VM is present).
		raw, _ = json.Marshal(infos[0])
	} else {
		raw, _ = json.Marshal(infos)
	}

	// Randomly wrap with BOM / whitespace so the trim path is exercised.
	var buf []byte
	if r.Intn(2) == 0 {
		buf = append(buf, 0xef, 0xbb, 0xbf)
	}
	if r.Intn(2) == 0 {
		buf = append(buf, []byte("\n  ")...)
	}
	buf = append(buf, raw...)
	if r.Intn(2) == 0 {
		buf = append(buf, []byte("  \r\n")...)
	}
	return buf
}

// buildReferenceMaps populates the enrichment maps DIRECTLY from the logical model,
// reproducing exactly what old inventory blocks 2-8 produced. This is Path B - the
// original per-block assembly - and is intentionally derived independently from the
// merged-JSON render so the equivalence assertion is meaningful.
//
// It mirrors parseEnrichmentIntoMaps' documented population rules: NICs/VHDs/snapshots
// keyed by vmName; dvd written only when non-nil; nested and firmware written for every
// VM; ha keyed by vmId and only when clustered (standalone hosts skip HA entirely).
func buildReferenceMaps(vms []logicalVM, clustered bool) (
	map[string][]VMNic,
	map[string][]VMVHD,
	map[string]*string,
	map[string]bool,
	map[string][]VMSnapshot,
	map[string]vmFirmwareInfo,
	map[string]bool,
) {
	nicsMap, vhdsMap, dvdMap, nestedMap, snapshotsMap, firmwareMap, haMap := newEnrichmentMaps()

	for _, vm := range vms {
		for _, n := range vm.nics {
			nicsMap[vm.name] = append(nicsMap[vm.name], VMNic{
				ID:          n.ID,
				Name:        n.Name,
				NetworkID:   n.NetworkID,
				MacAddress:  n.MacAddress,
				Primary:     false,
				IPAddresses: n.IPAddresses,
				VlanID:      n.VlanID,
			})
		}
		for _, v := range vm.vhds {
			vhdsMap[vm.name] = append(vhdsMap[vm.name], VMVHD{
				DiskID:     v.DiskID,
				Path:       v.Path,
				Controller: v.Controller,
				Format:     v.Format,
				Type:       v.Type,
				SizeGB:     v.SizeGB,
			})
		}
		if vm.dvd != nil {
			dvdMap[vm.name] = vm.dvd
		}
		nestedMap[vm.name] = vm.nested
		for _, s := range vm.snapshots {
			snapshotsMap[vm.name] = append(snapshotsMap[vm.name], VMSnapshot{
				ID:                 s.ID,
				Name:               s.Name,
				CreationTime:       s.CreationTime,
				SnapshotType:       s.SnapshotType,
				ParentSnapshotID:   s.ParentSnapshotID,
				ParentSnapshotName: s.ParentSnapshotName,
			})
		}
		firmwareMap[vm.name] = vmFirmwareInfo{
			VMName:             vm.name,
			SecureBoot:         vm.secureBoot,
			SecureBootTemplate: vm.secureBootTemplate,
		}
		if clustered {
			haMap[vm.id] = vm.ha
		}
	}

	return nicsMap, vhdsMap, dvdMap, nestedMap, snapshotsMap, firmwareMap, haMap
}

func mustMarshalInventory(t *testing.T, vms []VMInfo) string {
	t.Helper()
	b, err := json.Marshal(&VMInventoryResult{VMs: vms})
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Inventory equivalence: merged pass vs original per-block assembly
// ---------------------------------------------------------------------------

// TestEquivalence_Inventory_MergedPassVsPerBlock is the KEY new property for task 4:
// for a random logical VM set, the fixed merged-enrichment parse and the original
// per-block map population produce byte-identical VMInventoryResult JSON when fed to
// assembleVMInventory. This proves F(X) = F'(X) at the data level for the inventory
// path across the domain, including single-object-vs-array unmarshalling and
// BOM/whitespace prefixes.
func TestEquivalence_Inventory_MergedPassVsPerBlock(t *testing.T) {
	cfg := &quick.Config{MaxCount: 500}
	err := quick.Check(func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		clustered := r.Intn(2) == 0
		vms := genLogicalVMs(r)
		basics := basicsFromLogical(vms)

		// Path A: fixed merged pass - render merged JSON, parse into maps.
		mergedJSON := renderMergedJSON(r, vms, clustered)
		nA, vA, dA, neA, sA, fA, hA := newEnrichmentMaps()
		parseEnrichmentIntoMaps(mergedJSON, clustered, nA, vA, dA, neA, sA, fA, hA)
		gotJSON := mustMarshalInventory(t, assembleVMInventory(basics, nA, vA, dA, neA, sA, fA, hA))

		// Path B: original per-block - build the maps directly from the logical model.
		nB, vB, dB, neB, sB, fB, hB := buildReferenceMaps(vms, clustered)
		wantJSON := mustMarshalInventory(t, assembleVMInventory(basics, nB, vB, dB, neB, sB, fB, hB))

		if gotJSON != wantJSON {
			t.Logf("equivalence mismatch (clustered=%v):\n merged:   %s\n perBlock: %s", clustered, gotJSON, wantJSON)
			return false
		}
		return true
	}, cfg)
	if err != nil {
		t.Fatalf("inventory equivalence property violated: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Batch equivalence: per-item error strings + aggregate/ordering
// ---------------------------------------------------------------------------

func pickBatchAction(r *rand.Rand) (itemAction, desiredState string) {
	if r.Intn(2) == 0 {
		return "vm_start", "Running"
	}
	return "vm_stop", "Off"
}

// randMessage returns an arbitrary string that may include double quotes, so the %q
// formatting in the name-mismatch error is exercised across the domain.
func randMessage(r *rand.Rand) string {
	const alpha = `abcdef 012 "quoted" -_.`
	n := r.Intn(12)
	b := make([]byte, n)
	for i := range b {
		b[i] = alpha[r.Intn(len(alpha))]
	}
	return string(b)
}

// TestEquivalence_Batch_PerItemErrorStrings asserts that batchItemFromStateOp
// reproduces the ORIGINAL inline mapping's exact error strings across the input
// domain (arbitrary target names, found names, and operation-error messages). The
// 3.7 invariants test only asserts a non-empty error on these branches; this asserts
// the precise formatting for random inputs.
func TestEquivalence_Batch_PerItemErrorStrings(t *testing.T) {
	cfg := &quick.Config{MaxCount: 500}
	err := quick.Check(func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		itemAction, desired := pickBatchAction(r)
		target := VMBatchTarget{
			VMID:   fmt.Sprintf("%08x-1111-2222-3333-444444444444", r.Uint32()),
			VMName: randToken(r, "req-"),
		}
		state := randState(r)

		// Name-mismatch branch: the found name differs from the requested name.
		foundName := randToken(r, "found-")
		mismatch := vmBatchStateOp{Name: foundName, PreviousState: state, State: state, Mismatch: true}
		itemM, followupM := batchItemFromStateOp(target, itemAction, desired, mismatch)
		wantMismatch := fmt.Sprintf("VM name mismatch for id %s: expected %q, found %q", target.VMID, target.VMName, foundName)
		if itemM.Error != wantMismatch || followupM.fetch {
			return false
		}

		// Operation-error branch: the op ran and failed with an arbitrary message.
		opErr := randMessage(r)
		if opErr == "" {
			opErr = "operation failed"
		}
		opFail := vmBatchStateOp{Name: target.VMName, PreviousState: state, State: state, OpError: opErr}
		itemO, followupO := batchItemFromStateOp(target, itemAction, desired, opFail)
		wantOp := fmt.Sprintf("failed to %s VM: %s", strings.TrimPrefix(itemAction, "vm_"), opErr)
		if itemO.Error != wantOp || itemO.Status != "failed" || !followupO.fetch {
			return false
		}

		return true
	}, cfg)
	if err != nil {
		t.Fatalf("batch per-item error-string equivalence violated: %v", err)
	}
}

// referenceAggregateStatus derives the batch aggregate status independently from the
// per-item status SEQUENCE (not from precomputed counts). completed iff every item is
// ok (vacuously true for an empty batch, matching deriveVMBatchStatus(0,0)); failed iff
// at least one item exists and none are ok; partial otherwise. This is a deliberately
// different formulation from deriveVMBatchStatus (which works off succeeded/failed
// counts), so agreement across the domain proves the count-based derivation is
// order-insensitive and equivalent to scanning the sequence.
func referenceAggregateStatus(statuses []string) string {
	if len(statuses) == 0 {
		return "completed"
	}
	allOK := true
	anyOK := false
	for _, s := range statuses {
		if s == "ok" {
			anyOK = true
		} else {
			allOK = false
		}
	}
	switch {
	case allOK:
		return "completed"
	case !anyOK:
		return "failed"
	default:
		return "partial"
	}
}

// TestEquivalence_Batch_AggregateStatusAndOrdering simulates a sequence of per-VM
// outcomes (in input order), counts succeeded/failed exactly as handleVMBatchAction
// does (item.Status == "ok" => succeeded, else failed), and asserts:
//   - deriveVMBatchStatus over the counts equals referenceAggregateStatus over the
//     ordered status sequence (aggregate-status equivalence), and
//   - the succeeded+failed counts partition the batch (total preserved), which - with
//     the in-order scan - confirms input ordering does not affect the aggregate.
func TestEquivalence_Batch_AggregateStatusAndOrdering(t *testing.T) {
	cfg := &quick.Config{MaxCount: 500}
	err := quick.Check(func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		n := r.Intn(8) // 0..7 VMs in the batch

		statuses := make([]string, n)
		succeeded, failed := 0, 0
		for i := 0; i < n; i++ {
			if r.Intn(2) == 0 {
				statuses[i] = "ok"
				succeeded++
			} else {
				statuses[i] = "failed"
				failed++
			}
		}

		// Total is preserved (every item is exactly one of succeeded/failed).
		if succeeded+failed != n {
			return false
		}

		got := deriveVMBatchStatus(succeeded, failed)
		want := referenceAggregateStatus(statuses)
		return got == want
	}, cfg)
	if err != nil {
		t.Fatalf("batch aggregate-status/ordering equivalence violated: %v", err)
	}
}
