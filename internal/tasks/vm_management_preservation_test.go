package tasks

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"testing/quick"
)

// Feature: vm-powershell-optimization
// Property 2: Preservation - Functional Output Equivalence
//
// **Validates: Requirements 3.5, 3.6, 3.7 (management side)**
//
// PRESERVATION BASELINE tests for the VM management result mapping. Like the
// inventory handlers, the management handlers are coupled to hyperv.RunPowerShell,
// so only the host-independent result-mapping / aggregation / validation logic is
// captured here as golden output:
//   - newVMStopResult      : vm_stop result payload incl. the "Off" final-state default (2e)
//   - deriveVMBatchStatus   : aggregate completed/partial/failed derivation (2f)
//   - isCanonicalGUID       : batch GUID validation gate (2f)
//   - extractPSErrorDetail  : per-op error message preservation incl. markers (2f, 3.6)
// Full per-VM batch state transitions (previousState/state/changed against a live
// operation) are host-coupled and are covered by the integration matrix (task 5)
// and the merged-logic unit tests (task 3.7).
//
// EXPECTED OUTCOME ON UNFIXED CODE: these tests PASS (they define the baseline).

// ---------------------------------------------------------------------------
// 2e: vm_stop result payload golden (real newVMStopResult)
// ---------------------------------------------------------------------------

func TestPreservation_NewVMStopResult_Golden(t *testing.T) {
	const vmID = "22222222-2222-2222-2222-222222222222"
	const vmName = "db-01"

	tests := []struct {
		name       string
		state      string
		goldenJSON string
	}{
		{
			name:       "empty final state defaults to Off",
			state:      "",
			goldenJSON: `{"vmId":"22222222-2222-2222-2222-222222222222","vmName":"db-01","action":"vm_stop","status":"Off"}`,
		},
		{
			name:       "Off state preserved",
			state:      "Off",
			goldenJSON: `{"vmId":"22222222-2222-2222-2222-222222222222","vmName":"db-01","action":"vm_stop","status":"Off"}`,
		},
		{
			name:       "non-Off final state preserved verbatim",
			state:      "Running",
			goldenJSON: `{"vmId":"22222222-2222-2222-2222-222222222222","vmName":"db-01","action":"vm_stop","status":"Running"}`,
		},
		{
			name:       "Saved final state preserved verbatim",
			state:      "Saved",
			goldenJSON: `{"vmId":"22222222-2222-2222-2222-222222222222","vmName":"db-01","action":"vm_stop","status":"Saved"}`,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(newVMStopResult(vmID, vmName, tc.state))
			if err != nil {
				t.Fatalf("marshal failed: %v", err)
			}
			if string(got) != tc.goldenJSON {
				t.Fatalf("golden mismatch:\n got:  %s\n want: %s", string(got), tc.goldenJSON)
			}
		})
	}
}

// TestPreservation_NewVMStopResult_Invariants asserts the universal contract:
// the action is always "vm_stop" and the status is never empty (empty defaults to
// "Off"); any non-empty state is preserved unchanged.
func TestPreservation_NewVMStopResult_Invariants(t *testing.T) {
	cfg := &quick.Config{MaxCount: 300}
	err := quick.Check(func(vmID, vmName, state string) bool {
		res := newVMStopResult(vmID, vmName, state)
		if res.Action != "vm_stop" {
			return false
		}
		if res.VMID != vmID || res.VMName != vmName {
			return false
		}
		if state == "" {
			return res.Status == "Off"
		}
		return res.Status == state
	}, cfg)
	if err != nil {
		t.Fatalf("vm_stop result invariant violated: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 2f: batch aggregate status golden (real deriveVMBatchStatus)
// ---------------------------------------------------------------------------

func TestPreservation_DeriveVMBatchStatus_Golden(t *testing.T) {
	tests := []struct {
		name      string
		succeeded int
		failed    int
		want      string
	}{
		{"all succeeded => completed", 3, 0, "completed"},
		{"single succeeded => completed", 1, 0, "completed"},
		{"all failed => failed", 0, 3, "failed"},
		{"single failed => failed", 0, 1, "failed"},
		{"mixed => partial", 2, 1, "partial"},
		{"mixed one each => partial", 1, 1, "partial"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveVMBatchStatus(tc.succeeded, tc.failed); got != tc.want {
				t.Fatalf("deriveVMBatchStatus(%d,%d) = %q, want %q", tc.succeeded, tc.failed, got, tc.want)
			}
		})
	}
}

// TestPreservation_DeriveVMBatchStatus_Invariants asserts the derivation precedence
// over arbitrary non-negative counts with at least one item (batches require >= 1 VM):
// no failures => completed; no successes => failed; otherwise partial.
func TestPreservation_DeriveVMBatchStatus_Invariants(t *testing.T) {
	cfg := &quick.Config{MaxCount: 300}
	err := quick.Check(func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		succeeded := r.Intn(50)
		failed := r.Intn(50)
		if succeeded+failed == 0 {
			succeeded = 1 // batches always have >= 1 processed VM
		}
		got := deriveVMBatchStatus(succeeded, failed)
		switch {
		case failed == 0:
			return got == "completed"
		case succeeded == 0:
			return got == "failed"
		default:
			return got == "partial"
		}
	}, cfg)
	if err != nil {
		t.Fatalf("batch status derivation invariant violated: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 2f: batch GUID validation golden (real isCanonicalGUID)
// ---------------------------------------------------------------------------

func TestPreservation_IsCanonicalGUID_Golden(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{"valid lowercase guid", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", true},
		{"valid uppercase guid", "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA", true},
		{"valid mixed hex digits", "12345678-9abc-def0-1234-567890abcdef", true},
		{"too short", "1234", false},
		{"missing dashes", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false},
		{"dash in wrong position", "aaaaaaaaa-aaa-aaaa-aaaa-aaaaaaaaaaaa", false},
		{"non-hex character", "gggggggg-aaaa-aaaa-aaaa-aaaaaaaaaaaa", false},
		{"empty string", "", false},
		{"braced guid rejected", "{aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa}", false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := isCanonicalGUID(tc.value); got != tc.want {
				t.Fatalf("isCanonicalGUID(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 2f / 3.6: management error-message preservation golden (real extractPSErrorDetail)
// ---------------------------------------------------------------------------

func TestPreservation_ExtractPSErrorDetail_Golden(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		markers []string
		want    string
	}{
		{
			name: "strips powershell error prefix",
			err:  fmt.Errorf("powershell error: something went wrong"),
			want: "something went wrong",
		},
		{
			name: "strips powershell execution failed prefix",
			err:  fmt.Errorf("powershell execution failed: boom"),
			want: "boom",
		},
		{
			name:    "strips known marker and leading colon",
			err:     fmt.Errorf("ENABLE_HA_FAILED: cluster unavailable"),
			markers: []string{"ENABLE_HA_FAILED:"},
			want:    "cluster unavailable",
		},
		{
			name:    "marker embedded after prefix",
			err:     fmt.Errorf("powershell error: SNAPSHOT_CREATE_FAILED: disk full"),
			markers: []string{"SNAPSHOT_CREATE_FAILED:"},
			want:    "disk full",
		},
		{
			name: "empty message returns fallback",
			err:  fmt.Errorf(""),
			want: "unknown error (no details from PowerShell)",
		},
		{
			name: "plain message returned trimmed",
			err:  fmt.Errorf("  VM is not off  "),
			want: "VM is not off",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := extractPSErrorDetail(tc.err, tc.markers...); got != tc.want {
				t.Fatalf("extractPSErrorDetail = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPreservation_EscapePSSingleQuoted_Golden pins the single-quote doubling used
// when interpolating VM ids/names into single-quoted PowerShell literals (batch and
// state scripts), which is part of preserving the exact executed commands.
func TestPreservation_EscapePSSingleQuoted_Golden(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"plain", "plain"},
		{"O'Brien", "O''Brien"},
		{"''", "''''"},
		{"", ""},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			if got := escapePSSingleQuoted(tc.in); got != tc.want {
				t.Fatalf("escapePSSingleQuoted(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
