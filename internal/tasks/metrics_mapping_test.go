package tasks

import "testing"

// Wire-contract tests for the quick-metrics feature: the flat backend `function`
// names must resolve to the metrics task types, and StatePayload must route the
// results to the <hostid>.host_metrics / <hostid>.vm_metrics queues.

func TestResolveFunction_Metrics(t *testing.T) {
	cases := map[string]TaskType{
		"host_metrics":       TaskHostMetrics,
		"vm_metrics":         TaskVMMetrics,
		"vm_enable_metrics":  TaskVMManagement,
		"vm_disable_metrics": TaskVMManagement,
	}
	for fn, wantType := range cases {
		gotType, gotAction := ResolveFunction(fn)
		if gotType != wantType {
			t.Errorf("ResolveFunction(%q) type = %q, want %q", fn, gotType, wantType)
		}
		if wantType == TaskVMManagement && gotAction != fn {
			t.Errorf("ResolveFunction(%q) action = %q, want %q", fn, gotAction, fn)
		}
	}
}

func TestStatePayload_HostMetrics(t *testing.T) {
	cpu := 12.5
	res := &HostMetricsResult{CPUPercent: &cpu, Disks: []HostDiskMetric{}, Net: []HostNetMetric{}}

	kind, payload, ok := StatePayload(TaskHostMetrics, res)
	if !ok || kind != "host_metrics" {
		t.Fatalf("StatePayload(host_metrics) = %q, ok=%v", kind, ok)
	}
	if payload == nil {
		t.Fatalf("unexpected nil payload")
	}
}

func TestStatePayload_VMMetrics(t *testing.T) {
	res := &VMMetricsResult{VMs: []VMMetric{{ID: "guid-1", Name: "app-01"}}}

	kind, _, ok := StatePayload(TaskVMMetrics, res)
	if !ok || kind != "vm_metrics" {
		t.Fatalf("StatePayload(vm_metrics) = %q, ok=%v", kind, ok)
	}
}

func TestUnmarshalList_Quirks(t *testing.T) {
	// null -> empty, non-nil
	if got := unmarshalList[VMMetric](nil); got == nil || len(got) != 0 {
		t.Errorf("unmarshalList(nil) = %v, want empty non-nil", got)
	}
	// single object -> one-element slice
	got := unmarshalList[HostNetMetric]([]byte(`{"name":"eth0","rxBps":10,"txBps":20}`))
	if len(got) != 1 || got[0].Name != "eth0" {
		t.Errorf("unmarshalList(single) = %+v, want one eth0 element", got)
	}
	// array -> slice
	got = unmarshalList[HostNetMetric]([]byte(`[{"name":"a"},{"name":"b"}]`))
	if len(got) != 2 {
		t.Errorf("unmarshalList(array) len = %d, want 2", len(got))
	}
}
