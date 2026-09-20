package tasks

// This file maps the flat `function` names on the wire (AgentRequest.function,
// defined by ovc-backend) onto the agent's internal task types + actions.
//
// The backend sends e.g. function "vm_start" with params {"vm_id": "..."};
// internally that is handled by the vm_management module with action "vm_start".
// Inventory/create/edit functions map 1:1 to a dedicated handler.

// vmManagementActions are the functions dispatched through handleVMManagement.
var vmManagementActions = map[string]bool{
	"vm_start": true, "vm_stop": true, "vm_shutdown": true, "vm_restart": true,
	"vm_pause": true, "vm_resume": true,
	"vm_delete": true, "vm_migrate": true,
	"snapshot_create": true, "snapshot_remove": true, "snapshot_restore": true,
	"mount_dvd": true, "eject_dvd": true,
	"enable_ha": true, "disable_ha": true,
	"vm_rename": true, "vm_move": true, "vm_export_template": true,
	"notes_edit": true, "refresh_status": true, "vm_startup_change": true,
	"vm_batch_start": true, "vm_batch_stop": true,
	"vm_enable_metrics": true, "vm_disable_metrics": true,
}

// hostManagementActions are the functions dispatched through handleHostManagement.
var hostManagementActions = map[string]bool{
	"host_restart_agent": true,
	"suspend":            true, "suspend_drain": true,
	"resume": true, "resume_fallback": true, "restart": true,
	"refresh_hardware": true, "refresh_inventory": true,
}

// resolveFunction returns the internal task type and action for a wire function.
func ResolveFunction(fn string) (taskType TaskType, action string) {
	switch {
	case vmManagementActions[fn]:
		return TaskVMManagement, fn
	case hostManagementActions[fn]:
		return TaskHostManagement, fn
	case fn == "host_update_agent":
		return TaskArtifactDownload, fn
	case fn == "host_hwinventory":
		return TaskHardwareInventory, ""
	case fn == "vm_create":
		return TaskVMCreate, ""
	case fn == "vm_edit":
		return TaskVMEdit, ""
	case fn == "vm_clone":
		return TaskVMClone, ""
	case fn == "vm_inventory":
		return TaskVMInventory, ""
	case fn == "vm_status":
		return TaskVMStatus, ""
	case fn == "agent_status":
		return TaskAgentStatus, ""
	case fn == "template_inventory":
		return TaskTemplateInventory, ""
	case fn == "iso_inventory":
		return TaskISOInventory, ""
	case fn == "host_metrics":
		return TaskHostMetrics, ""
	case fn == "vm_metrics":
		return TaskVMMetrics, ""
	default:
		return TaskType(fn), ""
	}
}

// wireFunction is the inverse used to echo `function` back in AgentResponse.
// The backend does not key logic on it (it matches on id), so an approximate
// name is acceptable, but we try to return the original function.
func wireFunction(taskType TaskType, action string) string {
	if action != "" {
		return action
	}
	switch taskType {
	case TaskHardwareInventory:
		return "host_hwinventory"
	default:
		return string(taskType)
	}
}
