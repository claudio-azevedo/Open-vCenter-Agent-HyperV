package tasks

import (
	"testing"
	"testing/quick"
)

// Feature: vm-powershell-optimization (task 3.7)
//
// Focused unit tests for batchItemFromStateOp, the pure mapping helper extracted
// from processVMBatchVM (task 3.5). It maps a parsed vmBatchStateOp - the result of
// the combined per-VM state+operation script - into the base VMBatchItemResult and a
// batchStatusFollowup, WITHOUT any PowerShell spawn or vmStatus fetch. These tests
// close the gap left by the task-2 preservation baselines, which covered
// newVMStopResult / deriveVMBatchStatus / isCanonicalGUID / extractPSErrorDetail /
// escapePSSingleQuoted but NOT the combined batch state+operation branch mapping.
//
// Covered branches (Requirements 3.5, 3.6, 3.7):
//   - name-mismatch      : Mismatch=true -> exact mismatch error, no status fetch
//   - operation-error    : OpError set  -> "failed to start/stop VM: ..." + fetch/apply
//   - already-in-state   : Changed=false -> Status ok, not changed, fetch/no-apply
//   - success            : Changed=true  -> Status ok, changed, fetch/apply-with-default
//
// The subsequent vmStatus reconciliation (nil vs non-nil handleVMStatus result) is
// host-coupled and stays in processVMBatchVM; it is exercised by the integration
// matrix (task 5). Here we assert the pure mapping and the followup directive that
// drives that reconciliation.

func startTarget() VMBatchTarget {
	return VMBatchTarget{VMID: "33333333-3333-3333-3333-333333333333", VMName: "app-01"}
}

func TestBatchItemFromStateOp_NameMismatch(t *testing.T) {
	target := startTarget()
	combined := vmBatchStateOp{
		Name:          "someone-else",
		PreviousState: "Running",
		State:         "Running",
		Mismatch:      true,
	}

	item, followup := batchItemFromStateOp(target, "vm_stop", "Off", combined)

	if followup.fetch {
		t.Fatalf("name-mismatch must not trigger a status fetch, got followup=%+v", followup)
	}
	if item.Status != "failed" {
		t.Fatalf("name-mismatch status = %q, want failed", item.Status)
	}
	if item.Changed {
		t.Fatalf("name-mismatch must not be marked changed")
	}
	// VMName stays the requested target name (NOT the mismatched found name).
	if item.VMName != target.VMName {
		t.Fatalf("name-mismatch VMName = %q, want %q", item.VMName, target.VMName)
	}
	if item.PreviousState != "Running" || item.State != "Running" {
		t.Fatalf("name-mismatch state fields = prev %q / state %q, want Running/Running", item.PreviousState, item.State)
	}
	wantErr := `VM name mismatch for id 33333333-3333-3333-3333-333333333333: expected "app-01", found "someone-else"`
	if item.Error != wantErr {
		t.Fatalf("name-mismatch error:\n got:  %s\n want: %s", item.Error, wantErr)
	}
}

func TestBatchItemFromStateOp_OperationError_Start(t *testing.T) {
	target := startTarget()
	combined := vmBatchStateOp{
		Name:          "app-01",
		PreviousState: "Off",
		State:         "Off",
		Changed:       false,
		OpError:       "not enough memory",
	}

	item, followup := batchItemFromStateOp(target, "vm_start", "Running", combined)

	if !followup.fetch || !followup.applyState || followup.defaultState != "" {
		t.Fatalf("op-error followup = %+v, want fetch+applyState with no default", followup)
	}
	if item.Status != "failed" {
		t.Fatalf("op-error status = %q, want failed", item.Status)
	}
	if item.Changed {
		t.Fatalf("op-error must not be marked changed")
	}
	// VMName adopts the confirmed name from the script.
	if item.VMName != "app-01" {
		t.Fatalf("op-error VMName = %q, want app-01", item.VMName)
	}
	if item.PreviousState != "Off" || item.State != "Off" {
		t.Fatalf("op-error state fields = prev %q / state %q, want Off/Off", item.PreviousState, item.State)
	}
	// vm_start -> "failed to start VM: ..."
	wantErr := "failed to start VM: not enough memory"
	if item.Error != wantErr {
		t.Fatalf("op-error message:\n got:  %s\n want: %s", item.Error, wantErr)
	}
}

func TestBatchItemFromStateOp_OperationError_Stop(t *testing.T) {
	target := startTarget()
	combined := vmBatchStateOp{
		Name:          "app-01",
		PreviousState: "Running",
		State:         "Running",
		OpError:       "shutdown refused by guest",
	}

	item, _ := batchItemFromStateOp(target, "vm_stop", "Off", combined)

	// vm_stop -> "failed to stop VM: ..." (the "vm_" prefix is trimmed).
	wantErr := "failed to stop VM: shutdown refused by guest"
	if item.Error != wantErr {
		t.Fatalf("op-error message:\n got:  %s\n want: %s", item.Error, wantErr)
	}
}

func TestBatchItemFromStateOp_AlreadyInDesiredState(t *testing.T) {
	target := startTarget()
	combined := vmBatchStateOp{
		Name:          "app-01",
		PreviousState: "Off",
		State:         "Off",
		Changed:       false, // already off, op not run
	}

	item, followup := batchItemFromStateOp(target, "vm_stop", "Off", combined)

	// Fetch status for progress but keep the read state (do not overwrite).
	if !followup.fetch || followup.applyState {
		t.Fatalf("already-in-state followup = %+v, want fetch with applyState=false", followup)
	}
	if item.Status != "ok" {
		t.Fatalf("already-in-state status = %q, want ok", item.Status)
	}
	if item.Changed {
		t.Fatalf("already-in-state must report changed=false")
	}
	if item.Error != "" {
		t.Fatalf("already-in-state must have no error, got %q", item.Error)
	}
	if item.PreviousState != "Off" || item.State != "Off" {
		t.Fatalf("already-in-state state fields = prev %q / state %q, want Off/Off", item.PreviousState, item.State)
	}
}

func TestBatchItemFromStateOp_Success(t *testing.T) {
	target := startTarget()
	combined := vmBatchStateOp{
		Name:          "app-01",
		PreviousState: "Off",
		State:         "Off",
		Changed:       true, // start ran successfully
	}

	item, followup := batchItemFromStateOp(target, "vm_start", "Running", combined)

	// Success path fetches status and applies it, defaulting to the desired state
	// if the status query returns nothing.
	if !followup.fetch || !followup.applyState || followup.defaultState != "Running" {
		t.Fatalf("success followup = %+v, want fetch+applyState+default=Running", followup)
	}
	if item.Status != "ok" {
		t.Fatalf("success status = %q, want ok", item.Status)
	}
	if !item.Changed {
		t.Fatalf("success must report changed=true")
	}
	if item.Error != "" {
		t.Fatalf("success must have no error, got %q", item.Error)
	}
	// PreviousState is the read state; State is left at the read value until the
	// caller reconciles the fetched VMInfo (default Running when fetch is empty).
	if item.PreviousState != "Off" {
		t.Fatalf("success previousState = %q, want Off", item.PreviousState)
	}
}

// TestBatchItemFromStateOp_Invariants asserts the universal contract of the mapping
// across arbitrary state-op inputs, mirroring the branch precedence the original
// inline logic enforced:
//   - Action is always the passed itemAction; VMID always the target id.
//   - PreviousState always equals the script's PreviousState.
//   - Mismatch dominates (no fetch, failed, VMName stays the requested name).
//   - Otherwise VMName adopts the confirmed name; OpError => failed, else ok.
//   - "changed" is true only on the success branch (no mismatch, no op-error, Changed).
func TestBatchItemFromStateOp_Invariants(t *testing.T) {
	actions := []struct{ action, desired string }{
		{"vm_start", "Running"},
		{"vm_stop", "Off"},
	}

	cfg := &quick.Config{MaxCount: 500}
	check := func(seedName string, prev string, changed, mismatch bool, opErr string, ai int) bool {
		idx := ai % len(actions)
		if idx < 0 {
			idx += len(actions)
		}
		sel := actions[idx]
		target := VMBatchTarget{VMID: "44444444-4444-4444-4444-444444444444", VMName: "req-name"}
		combined := vmBatchStateOp{
			Name:          seedName,
			PreviousState: prev,
			State:         prev,
			Changed:       changed,
			OpError:       opErr,
			Mismatch:      mismatch,
		}

		item, followup := batchItemFromStateOp(target, sel.action, sel.desired, combined)

		if item.Action != sel.action || item.VMID != target.VMID {
			return false
		}
		if item.PreviousState != prev {
			return false
		}

		if mismatch {
			return !followup.fetch &&
				item.Status == "failed" &&
				!item.Changed &&
				item.VMName == target.VMName &&
				item.Error != ""
		}

		// Non-mismatch always adopts the confirmed name and fetches status.
		if item.VMName != seedName || !followup.fetch {
			return false
		}

		if opErr != "" {
			return item.Status == "failed" &&
				!item.Changed &&
				followup.applyState &&
				followup.defaultState == "" &&
				item.Error != ""
		}

		if !changed {
			// already-in-state
			return item.Status == "ok" &&
				!item.Changed &&
				item.Error == "" &&
				!followup.applyState
		}

		// success
		return item.Status == "ok" &&
			item.Changed &&
			item.Error == "" &&
			followup.applyState &&
			followup.defaultState == sel.desired
	}

	if err := quick.Check(check, cfg); err != nil {
		t.Fatalf("batch state-op mapping invariant violated: %v", err)
	}
}
