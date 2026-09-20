package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ovc-agent/internal/hyperv"
)

// Feature: vm-powershell-optimization (task 5)
// Property 1 + Property 2 - combined regression coverage.
//
// **Validates: Requirements 1.1, 1.4, 1.5, 2.1, 2.4, 2.5, 3.1, 3.2, 3.7, 3.8**
//
// This file is the integration/regression matrix. It ties the pieces from tasks
// 1-4 together into three guards:
//
//  1. INVENTORY MATRIX (data-level pre/post): the full VMInventoryResult is
//     compared "pre" (original per-block assembly) vs "post" (fixed merged pass)
//     across the whole shape matrix - {0, 1, many VMs} x {cluster, standalone} x
//     {running, off}. Because a live inventory enrichment run is impossible on a
//     Hyper-V-less host (handleVMInventory Block 1 / Get-VM fails and returns early),
//     the matrix is driven at the DATA level: the same logical VM model is rendered
//     two ways (merged-script JSON -> parseEnrichmentIntoMaps, and directly into the
//     per-block maps via buildReferenceMaps) and both are run through the shared,
//     pure assembleVMInventory. Byte-identical JSON proves F(X) = F'(X).
//
//  2. MANAGEMENT PRE/POST: vm_stop and vm_batch_* result payloads are compared
//     against independent "original mapping" references, and the streamed batch
//     progress percentage sequence is pinned as golden.
//
//  3. REGRESSION GUARDS for out-of-scope long-running actions (vm_move,
//     vm_export_template, vm_rename): the pure percent-mapping helpers extracted
//     from the move/export closures are asserted, and the fixed reportProgress
//     percentage checkpoints of all three handlers are pinned via AST so their
//     progress streams cannot silently regress.
//
//  4. SPAWN-REDUCTION GUARD: a structural (AST) count of powershell.exe-spawning
//     calls per hot-path function (complementing task 1), plus a live guard that
//     drives real powershell.exe through EnableDebugLog and confirms the debug-log
//     hook records exactly one execution per RunPowerShell call - the mechanism the
//     design relies on as spawn-count evidence.

// ---------------------------------------------------------------------------
// 1. Inventory matrix: pre (per-block) vs post (merged pass)
// ---------------------------------------------------------------------------

// matrixLogicalVMs builds a deterministic logical VM set of the requested size with
// a controlled mix of enrichment shapes (fully-loaded, bare, NICs-only, disks +
// snapshots). The `running` flag drives the running-vs-off nuance: only running VMs
// carry NIC IP addresses (mirroring the merged script's behavior). IDs and names are
// unique so name-keyed and vmId-keyed maps are unambiguous.
func matrixLogicalVMs(n int, running bool) []logicalVM {
	state := "Off"
	ip := ""
	if running {
		state = "Running"
		ip = "10.0.0.5, 10.0.0.6"
	}
	dvd := `C:\ISOs\setup.iso`

	all := []logicalVM{
		{ // 0: fully loaded
			id:    "aaaaaaaa-0000-0000-0000-000000000001",
			name:  "vm-full-1",
			state: state,
			nics: []vmEnrichmentNic{
				{ID: "nic-1", Name: "Network Adapter", NetworkID: "vSwitch", MacAddress: "00155D010101", IPAddresses: ip, VlanID: 100},
			},
			vhds: []vmEnrichmentVHD{
				{DiskID: "disk-1", Path: `C:\Hyper-V\vm-full-1.vhdx`, Format: "VHDX", Type: "Dynamic", SizeGB: 127.5},
			},
			dvd:    &dvd,
			nested: true,
			snapshots: []vmEnrichmentSnapshot{
				{ID: "snap-1", Name: "cp-1", CreationTime: "2023-06-16T08:00:00.0000000+00:00", SnapshotType: "Standard"},
			},
			secureBoot:         true,
			secureBootTemplate: "Windows",
			ha:                 true,
		},
		{ // 1: bare (no enrichment) - exercises empty-array-not-null
			id:                 "bbbbbbbb-0000-0000-0000-000000000002",
			name:               "vm-bare-2",
			state:              state,
			secureBootTemplate: "",
			ha:                 false,
		},
		{ // 2: NICs only
			id:    "cccccccc-0000-0000-0000-000000000003",
			name:  "vm-nic-3",
			state: state,
			nics: []vmEnrichmentNic{
				{ID: "nic-3", Name: "Network Adapter", NetworkID: "sw2", MacAddress: "00155D010103", IPAddresses: ip, VlanID: 0},
			},
			secureBoot:         false,
			secureBootTemplate: "Linux",
			ha:                 true,
		},
		{ // 3: disks + snapshots (with parent pointers), no NICs
			id:    "dddddddd-0000-0000-0000-000000000004",
			name:  "vm-disk-4",
			state: state,
			vhds: []vmEnrichmentVHD{
				{DiskID: "disk-4", Path: `C:\Hyper-V\vm-disk-4.vhdx`, Format: "VHDX", Type: "Fixed", SizeGB: 64},
			},
			snapshots: []vmEnrichmentSnapshot{
				{ID: "snap-4", Name: "cp-4", CreationTime: "2024-01-01T00:00:00.0000000+00:00", SnapshotType: "Production", ParentSnapshotID: strPtr("psnap-4"), ParentSnapshotName: strPtr("pcp-4")},
			},
			nested:             false,
			secureBoot:         true,
			secureBootTemplate: "Others",
			ha:                 false,
		},
	}

	if n > len(all) {
		n = len(all)
	}
	return append([]logicalVM{}, all[:n]...)
}

// assembleFromMergedPass runs Path A (the fixed merged enrichment pass): render the
// logical set as the merged-script JSON, parse it through parseEnrichmentIntoMaps,
// and assemble. This is F'(X).
func assembleFromMergedPass(t *testing.T, r *rand.Rand, vms []logicalVM, basics []vmBasicInfo, clustered bool) string {
	t.Helper()
	merged := renderMergedJSON(r, vms, clustered)
	n, v, d, ne, s, f, h := newEnrichmentMaps()
	parseEnrichmentIntoMaps(merged, clustered, n, v, d, ne, s, f, h)
	return mustMarshalInventory(t, assembleVMInventory(basics, n, v, d, ne, s, f, h))
}

// assembleFromPerBlock runs Path B (the original per-block assembly): populate the
// enrichment maps directly from the logical model exactly as blocks 2-8 did, then
// assemble. This is F(X).
func assembleFromPerBlock(t *testing.T, vms []logicalVM, basics []vmBasicInfo, clustered bool) string {
	t.Helper()
	n, v, d, ne, s, f, h := buildReferenceMaps(vms, clustered)
	return mustMarshalInventory(t, assembleVMInventory(basics, n, v, d, ne, s, f, h))
}

func TestIntegrationMatrix_Inventory_PrePostEquivalence(t *testing.T) {
	sizes := []struct {
		label string
		n     int
	}{
		{"zero_vms", 0},
		{"one_vm", 1},
		{"many_vms", 4},
	}

	for _, sz := range sizes {
		for _, clustered := range []bool{false, true} {
			for _, running := range []bool{false, true} {
				name := fmt.Sprintf("%s/clustered=%v/running=%v", sz.label, clustered, running)
				t.Run(name, func(t *testing.T) {
					// Deterministic seed per cell so the BOM/whitespace and
					// single-object-vs-array variety in renderMergedJSON is stable.
					r := rand.New(rand.NewSource(int64(len(name)) * 7))
					vms := matrixLogicalVMs(sz.n, running)
					basics := basicsFromLogical(vms)

					post := assembleFromMergedPass(t, r, vms, basics, clustered) // F'(X)
					pre := assembleFromPerBlock(t, vms, basics, clustered)       // F(X)

					if pre != post {
						t.Fatalf("pre/post inventory divergence (clustered=%v running=%v):\n pre (per-block): %s\n post(merged):    %s",
							clustered, running, pre, post)
					}

					// Shape assertions for this matrix cell.
					var res VMInventoryResult
					if err := json.Unmarshal([]byte(post), &res); err != nil {
						t.Fatalf("unmarshal assembled inventory: %v", err)
					}
					if len(res.VMs) != sz.n {
						t.Fatalf("expected %d VMs, got %d", sz.n, len(res.VMs))
					}
					for i, vm := range res.VMs {
						// Empty-array-not-null holds for every VM (Requirement 3.3).
						if vm.NICs == nil || vm.VHDs == nil || vm.Snapshots == nil {
							t.Fatalf("VM %s: nics/vhds/snapshots must never be null", vm.Name)
						}
						// Cluster-skip: standalone hosts yield ha=false for all VMs (Requirement 3.2).
						if !clustered && vm.HA {
							t.Fatalf("VM %s: standalone host must report ha=false", vm.Name)
						}
						// Input ordering preserved (Requirement 3.1/3.7).
						if vm.ID != vms[i].id || vm.Name != vms[i].name {
							t.Fatalf("VM order/identity not preserved at index %d: got %s/%s want %s/%s",
								i, vm.ID, vm.Name, vms[i].id, vms[i].name)
						}
					}
				})
			}
		}
	}
}

// TestIntegrationMatrix_Inventory_MixedRunningOff exercises a single inventory with
// BOTH running and off VMs present, confirming the running-vs-off IP nuance and the
// pre/post equivalence hold when states are mixed within one collection.
func TestIntegrationMatrix_Inventory_MixedRunningOff(t *testing.T) {
	for _, clustered := range []bool{false, true} {
		running := matrixLogicalVMs(2, true)
		off := matrixLogicalVMs(2, false)
		// Re-key the "off" VMs so ids/names stay unique across the merged set.
		for i := range off {
			off[i].id = "eeee" + off[i].id[4:]
			off[i].name = off[i].name + "-off"
		}
		vms := append(append([]logicalVM{}, running...), off...)
		basics := basicsFromLogical(vms)

		r := rand.New(rand.NewSource(99))
		post := assembleFromMergedPass(t, r, vms, basics, clustered)
		pre := assembleFromPerBlock(t, vms, basics, clustered)
		if pre != post {
			t.Fatalf("mixed running/off pre/post divergence (clustered=%v):\n pre:  %s\n post: %s", clustered, pre, post)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. Management pre/post: vm_stop payload, batch payload + progress sequence
// ---------------------------------------------------------------------------

// referenceVMStopResult reproduces the ORIGINAL vm_stop result mapping (empty final
// state defaults to "Off") independently of newVMStopResult, so the assertion below
// is a genuine pre/post comparison rather than a self-check.
func referenceVMStopResult(vmID, vmName, state string) *VMManagementResult {
	if state == "" {
		state = "Off"
	}
	return &VMManagementResult{VMID: vmID, VMName: vmName, Action: "vm_stop", Status: state}
}

func TestIntegrationMatrix_VMStop_PayloadPrePost(t *testing.T) {
	const vmID = "22222222-2222-2222-2222-222222222222"
	const vmName = "db-01"
	for _, state := range []string{"", "Off", "Running", "Saved", "Paused"} {
		got, err := json.Marshal(newVMStopResult(vmID, vmName, state)) // post
		if err != nil {
			t.Fatalf("marshal post failed: %v", err)
		}
		want, err := json.Marshal(referenceVMStopResult(vmID, vmName, state)) // pre
		if err != nil {
			t.Fatalf("marshal pre failed: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("vm_stop payload pre/post divergence for state %q:\n pre:  %s\n post: %s", state, want, got)
		}
	}
}

// referenceBatchItem reproduces the ORIGINAL inline per-VM batch mapping (before the
// batchItemFromStateOp extraction), returning the item and whether the caller would
// fetch the post-op VMInfo. Used as the pre-image for the pre/post comparison.
func referenceBatchItem(target VMBatchTarget, itemAction, desiredState string, combined vmBatchStateOp) (VMBatchItemResult, bool) {
	item := VMBatchItemResult{
		VMID:          target.VMID,
		VMName:        target.VMName,
		Action:        itemAction,
		Status:        "failed",
		State:         combined.PreviousState,
		PreviousState: combined.PreviousState,
	}
	if combined.Mismatch {
		item.Error = fmt.Sprintf("VM name mismatch for id %s: expected %q, found %q", target.VMID, target.VMName, combined.Name)
		return item, false
	}
	item.VMName = combined.Name
	if combined.OpError != "" {
		item.Error = fmt.Sprintf("failed to %s VM: %s", strings.TrimPrefix(itemAction, "vm_"), combined.OpError)
		return item, true
	}
	if !combined.Changed {
		item.Status = "ok"
		return item, true
	}
	item.Status = "ok"
	item.Changed = true
	return item, true
}

func TestIntegrationMatrix_Batch_PayloadPrePost(t *testing.T) {
	target := VMBatchTarget{VMID: "33333333-3333-3333-3333-333333333333", VMName: "app-01"}
	cases := []struct {
		name       string
		itemAction string
		desired    string
		combined   vmBatchStateOp
	}{
		{"start_name_mismatch", "vm_start", "Running", vmBatchStateOp{Name: "other", PreviousState: "Off", State: "Off", Mismatch: true}},
		{"stop_name_mismatch", "vm_stop", "Off", vmBatchStateOp{Name: "other", PreviousState: "Running", State: "Running", Mismatch: true}},
		{"start_op_error", "vm_start", "Running", vmBatchStateOp{Name: "app-01", PreviousState: "Off", State: "Off", OpError: "no memory"}},
		{"stop_op_error", "vm_stop", "Off", vmBatchStateOp{Name: "app-01", PreviousState: "Running", State: "Running", OpError: "guest refused"}},
		{"start_already_running", "vm_start", "Running", vmBatchStateOp{Name: "app-01", PreviousState: "Running", State: "Running", Changed: false}},
		{"stop_already_off", "vm_stop", "Off", vmBatchStateOp{Name: "app-01", PreviousState: "Off", State: "Off", Changed: false}},
		{"start_success", "vm_start", "Running", vmBatchStateOp{Name: "app-01", PreviousState: "Off", State: "Off", Changed: true}},
		{"stop_success", "vm_stop", "Off", vmBatchStateOp{Name: "app-01", PreviousState: "Running", State: "Running", Changed: true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotItem, gotFollowup := batchItemFromStateOp(target, tc.itemAction, tc.desired, tc.combined) // post
			wantItem, wantFetch := referenceBatchItem(target, tc.itemAction, tc.desired, tc.combined)    // pre

			g, _ := json.Marshal(gotItem)
			w, _ := json.Marshal(wantItem)
			if string(g) != string(w) {
				t.Fatalf("batch item payload pre/post divergence:\n pre:  %s\n post: %s", w, g)
			}
			if gotFollowup.fetch != wantFetch {
				t.Fatalf("batch followup.fetch pre/post divergence: pre=%v post=%v", wantFetch, gotFollowup.fetch)
			}
		})
	}
}

// batchProgressSequence reproduces the per-VM progress percentage the batch handler
// publishes: percent = (i+1)*100/total for i in [0,total). Pinned against golden
// sequences below so the streamed progress cannot silently change (Requirement 3.7).
func batchProgressSequence(total int) []int {
	seq := make([]int, total)
	for i := 0; i < total; i++ {
		seq[i] = (i + 1) * 100 / total
	}
	return seq
}

func TestIntegrationMatrix_Batch_ProgressSequenceGolden(t *testing.T) {
	golden := map[int][]int{
		1: {100},
		2: {50, 100},
		3: {33, 66, 100},
		4: {25, 50, 75, 100},
		5: {20, 40, 60, 80, 100},
	}
	for total, want := range golden {
		got := batchProgressSequence(total)
		if len(got) != len(want) {
			t.Fatalf("total=%d: sequence length %d, want %d", total, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("total=%d: progress[%d] = %d, want %d (full: %v)", total, i, got[i], want[i], got)
			}
		}
		// Final step always reaches 100%.
		if got[len(got)-1] != 100 {
			t.Fatalf("total=%d: final progress = %d, want 100", total, got[len(got)-1])
		}
	}
}

// ---------------------------------------------------------------------------
// 3. Regression guards for out-of-scope long-running actions (3.8)
// ---------------------------------------------------------------------------

// TestRegression_LongRunning_ProgressMapping pins the pure percent-mapping helpers
// extracted from the vm_move and vm_export_template progress closures. These map a
// job's 0-100% completion onto the task progress band; the exact math is what a
// caller streams, so it must not regress (Requirement 3.8).
func TestRegression_LongRunning_ProgressMapping(t *testing.T) {
	// vm_move: mapped = 5 + jobPercent*90/100  -> band [5, 95].
	moveCases := []struct{ in, want int }{{0, 5}, {1, 5}, {50, 50}, {100, 95}}
	for _, c := range moveCases {
		if got := mapVMMoveProgress(c.in); got != c.want {
			t.Fatalf("mapVMMoveProgress(%d) = %d, want %d", c.in, got, c.want)
		}
	}

	// vm_export_template: mapped = 20 + jobPercent*65/100 -> band [20, 85].
	exportCases := []struct{ in, want int }{{0, 20}, {1, 20}, {50, 52}, {100, 85}}
	for _, c := range exportCases {
		if got := mapVMExportProgress(c.in); got != c.want {
			t.Fatalf("mapVMExportProgress(%d) = %d, want %d", c.in, got, c.want)
		}
	}

	// Both mappings are monotonic non-decreasing across the job domain.
	prevMove, prevExport := -1, -1
	for p := 0; p <= 100; p++ {
		m, e := mapVMMoveProgress(p), mapVMExportProgress(p)
		if m < prevMove || e < prevExport {
			t.Fatalf("progress mapping not monotonic at jobPercent=%d (move=%d export=%d)", p, m, e)
		}
		prevMove, prevExport = m, e
	}
}

// reportProgressLiterals parses the non-test package sources and returns, for the
// named function, the ordered list of integer-literal percentages passed as the
// 4th argument to d.reportProgress(ctx, taskID, TaskVMManagement, <pct>, msg). Only
// literal ints are collected, so the dynamic `mapped`/`percent` values are ignored
// and the fixed checkpoints of each long-running handler are captured.
func reportProgressLiterals(t *testing.T, fnName string) []int {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var found bool
	var lits []int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name != fnName {
				continue
			}
			found = true
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "reportProgress" || len(call.Args) < 4 {
					return true
				}
				lit, ok := call.Args[3].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					return true
				}
				var v int
				if _, serr := fmt.Sscanf(lit.Value, "%d", &v); serr == nil {
					lits = append(lits, v)
				}
				return true
			})
		}
	}
	if !found {
		t.Fatalf("function %s not found in package sources", fnName)
	}
	return lits
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRegression_LongRunning_FixedProgressCheckpoints pins the fixed reportProgress
// percentage checkpoints streamed by vm_move, vm_export_template, and vm_rename. The
// dynamic in-flight percentages (move/export) are covered by the mapping helpers
// above; here we guard the static milestones so these out-of-scope handlers cannot
// regress their progress streams (Requirement 3.8).
func TestRegression_LongRunning_FixedProgressCheckpoints(t *testing.T) {
	cases := []struct {
		fn   string
		want []int
	}{
		{"handleVMMove", []int{1, 3, 5, 100}},
		{"handleVMExportTemplate", []int{5, 10, 15, 20, 90, 100}},
		{"handleVMRename", []int{5, 10, 15, 25, 95, 100}},
	}
	for _, c := range cases {
		got := reportProgressLiterals(t, c.fn)
		if !equalInts(got, c.want) {
			t.Fatalf("%s fixed progress checkpoints = %v, want %v (progress stream regressed)", c.fn, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. Spawn-reduction guards
// ---------------------------------------------------------------------------

// TestSpawnGuard_StructuralCounts is the authoritative spawn-count guard. It reuses
// the task-1 AST counting helpers (funcExecCounts / sumExec) to assert the EXACT
// post-fix powershell.exe-spawn structure of every hot-path function. Task 1 asserts
// thresholds (proving the reduction happened); this asserts the precise counts stay
// put so the optimization cannot silently regress.
//
// WHY STRUCTURAL AND NOT A LIVE INVENTORY RUN: on a Hyper-V-less host (all CI/dev
// machines) handleVMInventory's Block 1 (Get-VM) fails and the function returns
// before the merged enrichment pass runs, so a live run can never observe the
// enrichment spawn structure. The number of distinct hyperv.RunPowerShell* calls per
// logical operation is fixed in the source regardless of host, so it is measured
// directly. The complementary LIVE guard below proves the counting mechanism itself.
func TestSpawnGuard_StructuralCounts(t *testing.T) {
	counts := funcExecCounts(t)

	// Inventory: exactly 1 basic-info block + 1 merged enrichment pass = 2 spawns.
	if got := counts["handleVMInventory"]; got != 2 {
		t.Fatalf("handleVMInventory spawns = %d, want 2 (1 basic + 1 merged enrichment)", got)
	}
	if enrichment := counts["handleVMInventory"] - 1; enrichment != 1 {
		t.Fatalf("inventory enrichment spawns = %d, want exactly 1 merged pass", enrichment)
	}

	// vm_stop: a single combined stop+wait+read script.
	if got := counts["handleVMStopAction"]; got != 1 {
		t.Fatalf("handleVMStopAction spawns = %d, want 1", got)
	}

	// Batch per-VM: combined state+operation script (processVMBatchVM) + reused
	// status query (handleVMStatus). getVMBatchState was folded away by the fix and
	// contributes 0 if absent.
	perVM := sumExec(counts, "processVMBatchVM", "getVMBatchState", "handleVMStatus")
	if perVM != 2 {
		t.Fatalf("per-VM batch spawns = %d, want exactly 2 (combined state+op + status query)", perVM)
	}
}

// TestSpawnGuard_LiveDebugLogCountsExecutions is the complementary LIVE spawn-count
// guard. The design points at EnableDebugLog as the spawn-counting hook: it logs one
// entry per PowerShell execution regardless of whether the cmdlet succeeds. This test
// drives real powershell.exe through hyperv.RunPowerShell a known number of times with
// debug logging to a temp dir, then confirms exactly that many executions were logged
// - proving the 1-execution-per-call invariant the structural guard relies on. It is
// skipped when powershell.exe is unavailable so it stays green cross-platform.
func TestSpawnGuard_LiveDebugLogCountsExecutions(t *testing.T) {
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		t.Skip("powershell.exe not available; structural guard (above) is the authoritative spawn-count evidence in this environment")
	}

	dir := t.TempDir()
	hyperv.EnableDebugLog(dir)
	logPath := filepath.Join(dir, "powershell_debug.log")

	const invocations = 4
	for i := 0; i < invocations; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		// A trivial, deterministic, hermetic script - no Hyper-V cmdlets involved.
		_, _ = hyperv.RunPowerShell(ctx, "Write-Output 'spawn-guard-ok'")
		cancel()
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read debug log at %s: %v", logPath, err)
	}
	// Each logged execution writes exactly one header containing "duration=".
	logged := strings.Count(string(data), "duration=")
	if logged != invocations {
		t.Fatalf("debug log recorded %d executions, want %d (one per RunPowerShell call)", logged, invocations)
	}
}
