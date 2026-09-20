package tasks

import "ovc-agent/internal/protocol"

// StatePayload converts a completed inventory task result into the last-value
// queue payload ovc-backend expects (app/services/inventory.py) and the queue
// kind to publish it to. ok is false for task types that have no state queue.
//
//	agent_status       -> <hostid>.agent_status    { version, ..., hypervisor }
//	vm_inventory        -> <hostid>.vm_inventory    { reported_at, vms }
//	hardware_inventory  -> <hostid>.host_inventory  { reported_at, hardware }
//	                      (hardware is the canonical shape - HardwareInventoryResult
//	                       re-marshals itself, see marshal_hardware.go)
//	template_inventory  -> <hostid>.template_inventory { reported_at, items }
//	iso_inventory       -> <hostid>.iso_inventory   { reported_at, items }
func StatePayload(taskType TaskType, result interface{}) (kind string, payload interface{}, ok bool) {
	now := protocol.Now()

	switch taskType {
	case TaskAgentStatus:
		if r, isType := result.(*AgentStatusResult); isType && r != nil {
			return "agent_status", r.toPayload(now), true
		}
	case TaskVMInventory:
		vms := interface{}([]VMInfo{})
		if r, isType := result.(*VMInventoryResult); isType && r != nil {
			vms = r.VMs
		}
		return "vm_inventory", protocol.VMInventoryPayload{ReportedAt: now, VMs: vms}, true
	case TaskHardwareInventory:
		return "host_inventory", protocol.HostInventoryPayload{ReportedAt: now, Hardware: result}, true
	case TaskTemplateInventory:
		items := interface{}([]TemplateInfo{})
		if r, isType := result.(*TemplateInventoryResult); isType && r != nil {
			items = r.Templates
		}
		return "template_inventory", protocol.ImageInventoryPayload{ReportedAt: now, Items: items}, true
	case TaskISOInventory:
		items := interface{}([]ISOItem{})
		if r, isType := result.(*ISOInventoryResult); isType && r != nil {
			items = r.Items
		}
		return "iso_inventory", protocol.ImageInventoryPayload{ReportedAt: now, Items: items}, true
	case TaskHostMetrics:
		if r, isType := result.(*HostMetricsResult); isType && r != nil {
			return "host_metrics", r.toPayload(now), true
		}
	case TaskVMMetrics:
		vms := interface{}([]VMMetric{})
		if r, isType := result.(*VMMetricsResult); isType && r != nil {
			vms = r.VMs
		}
		return "vm_metrics", protocol.VMMetricsPayload{ReportedAt: now, VMs: vms}, true
	}

	return "", nil, false
}
