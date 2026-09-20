package tasks

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Feature: vm-powershell-optimization
// Property 1: Bug Condition - Reduced PowerShell Overhead with Identical Output
//
// **Validates: Requirements 1.1, 1.2, 1.3, 1.4, 1.5 (Bug Condition);
//              target 2.1, 2.2, 2.3, 2.4, 2.5 (Expected Behavior)**
//
// GOAL (exploration test): surface concrete, deterministic evidence of the current
// multi-process / per-VM-loop / multi-round-trip PowerShell structure that this
// optimization targets.
//
// WHY A STRUCTURAL (AST) COUNT INSTEAD OF A LIVE SPAWN COUNT:
// The design's preferred spawn-counting hook (EnableDebugLog in
// internal/hyperv/powershell.go) counts real powershell.exe executions. But on any
// host without Hyper-V (all CI / dev test hosts), handleVMInventory's Block 1
// (Get-VM basic info) fails and the function returns early - the 6-7 enrichment
// blocks are never reached, so a live run cannot observe the enrichment overhead at
// all. The bug is deterministic and STRUCTURAL: the number of distinct
// hyperv.RunPowerShell* invocations that make up one logical operation is fixed in
// the source regardless of host. We therefore measure that structure directly by
// parsing the package sources and counting the powershell.exe-spawning calls
// (hyperv.RunPowerShell / RunPowerShellRaw / RunPowerShellJSON / RunPowerShellStream /
// RunLongPowerShellStream) inside each hot-path function. This count is exactly the
// "process spawns per logical operation" the design promises to reduce, so the same
// assertions validate the fix once blocks are merged (they will PASS after task 3).
//
// EXPECTED OUTCOME ON UNFIXED CODE: these assertions FAIL. That failure is the
// success condition for this exploration test - it proves the avoidable overhead
// exists. DO NOT fix the code or the test when it fails here.

// psExecFuncs are the hyperv package functions that each spawn a powershell.exe process.
var psExecFuncs = map[string]bool{
	"RunPowerShell":           true,
	"RunPowerShellRaw":        true,
	"RunPowerShellJSON":       true,
	"RunPowerShellStream":     true,
	"RunLongPowerShellStream": true,
}

// Target (post-fix) spawn structure the optimization must reach. On the current
// (unfixed) code the real counts exceed these, so the assertions below fail.
const (
	// One merged enrichment pass, plus at most one conditional HA script when
	// clustered (blocks 2-8 collapse from 6-7 separate executions to <= 2).
	targetInventoryEnrichmentMax = 2
	// vm_stop: a single script that stops + waits + reads the final state.
	targetVMStopExecs = 1
	// vm_batch_*: combined state+operation script (1) + reused status query (1).
	targetBatchPerVMMax = 2
)

// funcExecCounts parses every non-test .go file in this package and returns a map
// from function name to the number of direct powershell.exe-spawning calls in that
// function's body. Counting is intentionally NON-transitive (direct calls only) so
// that a helper's own conditional fan-out (e.g. handleVMStatus falling back to
// handleVMInventory) does not pollute an unrelated operation's structural count;
// each logical operation is measured as an explicit set of functions instead.
func funcExecCounts(t *testing.T) map[string]int {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("failed to read package directory: %v", err)
	}

	fset := token.NewFileSet()
	counts := make(map[string]int)

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("failed to parse %s: %v", name, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}

			n := 0
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkgIdent, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				if pkgIdent.Name == "hyperv" && psExecFuncs[sel.Sel.Name] {
					n++
				}
				return true
			})

			counts[fn.Name.Name] += n
		}
	}

	return counts
}

// sumExec sums the direct powershell.exe-spawn counts of the named functions.
// Functions that are absent from the map (e.g. a helper removed by the fix)
// contribute 0, so the same helper set keeps working before and after the refactor.
func sumExec(counts map[string]int, names ...string) int {
	total := 0
	for _, name := range names {
		total += counts[name]
	}
	return total
}

// TestProperty_ReducedPowerShellOverhead is the Property 1 (Bug Condition) exploration
// test. Each case asserts the TARGET (reduced) spawn structure. On unfixed code every
// case fails, documenting the current overhead as a counterexample.
func TestProperty_ReducedPowerShellOverhead(t *testing.T) {
	counts := funcExecCounts(t)

	// --- Case A: inventory enrichment (blocks 2-8 must collapse to one merged pass) ---
	// _Requirements: 1.1, 1.2, 1.3 (Bug Condition); target 2.1, 2.2, 2.3_
	t.Run("A_inventory_enrichment_single_merged_pass", func(t *testing.T) {
		total, ok := counts["handleVMInventory"]
		if !ok {
			t.Fatal("handleVMInventory not found in package sources")
		}
		if total < 1 {
			t.Fatalf("expected at least the basic-info block in handleVMInventory, found %d spawns", total)
		}

		// The basic-info block (Block 1) is authoritative and stays standalone; every
		// other spawn in handleVMInventory is enrichment (blocks 2-8).
		enrichment := total - 1

		if enrichment > targetInventoryEnrichmentMax {
			t.Fatalf(
				"BUG CONDITION CONFIRMED (counterexample): handleVMInventory spawns %d PowerShell processes "+
					"(1 basic-info + %d enrichment). Target is <= %d enrichment spawns (one merged pass "+
					"+ at most one conditional HA script). The enrichment work is split across separate "+
					"per-VM-looping blocks (NICs-running, NICs-off, VHDs, HA, nested+DVD, snapshots, firmware) "+
					"instead of a single batched pass.",
				total, enrichment, targetInventoryEnrichmentMax,
			)
		}
	})

	// --- Case B: vm_stop must be a single stop+wait+read script ---
	// _Requirements: 1.4 (Bug Condition); target 2.4_
	t.Run("B_vm_stop_single_execution", func(t *testing.T) {
		got, ok := counts["handleVMStopAction"]
		if !ok {
			t.Fatal("handleVMStopAction not found in package sources")
		}

		if got != targetVMStopExecs {
			t.Fatalf(
				"BUG CONDITION CONFIRMED (counterexample): handleVMStopAction issues %d PowerShell "+
					"executions (Stop-VM, wait-loop, final-state read) for one logical stop. Target is "+
					"exactly %d combined script.",
				got, targetVMStopExecs,
			)
		}
	})

	// --- Case C: batch per-VM spawns must drop to <= 2 ---
	// _Requirements: 1.5 (Bug Condition); target 2.5_
	// One batch VM currently spawns: getVMBatchState (pre-state check) + the
	// start/stop operation script (in processVMBatchVM) + getVMBatchVMStatus, which
	// delegates its single spawn to handleVMStatus. We sum the direct spawns of that
	// explicit function set to measure per-VM process structure.
	t.Run("C_vm_batch_per_vm_at_most_two", func(t *testing.T) {
		if _, ok := counts["processVMBatchVM"]; !ok {
			t.Fatal("processVMBatchVM not found in package sources")
		}

		perVM := sumExec(counts,
			"processVMBatchVM", // start/stop operation script
			"getVMBatchState",  // pre-operation state + name check
			"handleVMStatus",   // post-operation full VMInfo (via getVMBatchVMStatus)
		)

		if perVM > targetBatchPerVMMax {
			t.Fatalf(
				"BUG CONDITION CONFIRMED (counterexample): a single batch VM spawns %d PowerShell "+
					"processes (state check + operation + status query); a 3-VM vm_batch_stop therefore "+
					"spawns ~%d. Target is <= %d per VM (combined state+operation + reused status query).",
				perVM, perVM*3, targetBatchPerVMMax,
			)
		}
	})
}
