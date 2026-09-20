// Package protocol mirrors the RabbitMQ message contract defined by ovc-backend
// in app/messaging/protocol.py and app/messaging/queues.py. The backend is the
// source of truth; keep these types in sync with it.
package protocol

import (
	"encoding/json"
	"time"
)

// Now returns an RFC3339 timestamp in UTC, matching the backend's isoformat.
func Now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// AgentRequest is the JSON the backend writes to <hostid>.request.
type AgentRequest struct {
	ID          string          `json:"id"` // == Task.id / AMQP correlation id
	Function    string          `json:"function"`
	Params      json.RawMessage `json:"params"`
	RequestedBy string          `json:"requested_by"`
	IssuedAt    string          `json:"issued_at"`
}

// AgentResponse is the JSON the agent writes to <hostid>.response.
//
// status is one of "running" | "succeeded" | "failed". The backend
// (services/inventory.py::apply_response) only persists result when it is a
// string; for VM state changes it reads vm_id + vm_state instead.
type AgentResponse struct {
	ID         string  `json:"id"`
	Function   string  `json:"function"`
	Status     string  `json:"status"`
	Progress   *int    `json:"progress"`
	Result     any     `json:"result"`
	Error      *string `json:"error"`
	VMID       *string `json:"vm_id"`
	VMState    *string `json:"vm_state"`
	FinishedAt *string `json:"finished_at"`
}

// Wire status values.
const (
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// NormalizeStatus maps the agent's internal lifecycle vocabulary onto the three
// values the backend understands.
func NormalizeStatus(s string) string {
	switch s {
	case "completed", "succeeded", "success", "ok":
		return StatusSucceeded
	case "failed", "error":
		return StatusFailed
	case "in_progress", "running", "pending", "queued":
		return StatusRunning
	default:
		return s
	}
}

// --- last-value inventory payloads (agent -> backend) ----------------------

type AgentStatusPayload struct {
	Version             string `json:"version"`
	VMRefreshInterval   int    `json:"vm_refresh_interval"`
	HostRefreshInterval int    `json:"host_refresh_interval"`
	Hypervisor          string `json:"hypervisor"`
	AgentType           string `json:"agent_type"`
	Hostname            string `json:"hostname,omitempty"`
	// FQDN and IP are agent-resolved. IP is the host's management/primary
	// address (the interface that owns the active default route) - never a
	// vMotion / live-migration / storage network.
	FQDN          string `json:"fqdn,omitempty"`
	IP            string `json:"ip,omitempty"`
	OSVersion     string `json:"os_version,omitempty"`
	UptimeSeconds int64  `json:"uptime_seconds,omitempty"`
	ReportedAt    string `json:"reported_at"`
}

type VMInventoryPayload struct {
	ReportedAt string `json:"reported_at"`
	VMs        any    `json:"vms"`
}

type HostInventoryPayload struct {
	ReportedAt string `json:"reported_at"`
	Hardware   any    `json:"hardware"`
}

type ImageInventoryPayload struct {
	ReportedAt string `json:"reported_at"`
	Items      any    `json:"items"`
}

// --- quick metrics payloads (short time-series queues, NOT last-value) --------
// Consumed by ovc-backend app/services/inventory.py::apply_host_metrics /
// apply_vm_metrics. Top-level reported_at is snake_case like the other payloads;
// the metric fields are camelCase like the inventory item shapes.

type HostMetricsPayload struct {
	ReportedAt    string   `json:"reported_at"`
	CPUPercent    *float64 `json:"cpuPercent"`
	MemPercent    *float64 `json:"memPercent"`
	MemUsedBytes  *int64   `json:"memUsedBytes"`
	MemTotalBytes *int64   `json:"memTotalBytes"`
	DiskLatencyMs *float64 `json:"diskLatencyMs"`
	NetRxBps      *int64   `json:"netRxBps"`
	NetTxBps      *int64   `json:"netTxBps"`
	Disks         any      `json:"disks"`
	Net           any      `json:"net"`
}

type VMMetricsPayload struct {
	ReportedAt string `json:"reported_at"`
	VMs        any    `json:"vms"`
}
