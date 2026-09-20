# ovc-agent - the Hyper-V agent for Open vCenter

This is the **Hyper-V** agent (`ovc-agent`, Go, build for WINDOWS only).
Other agents (libvirt / KVM) will follow that implement exactly the same
`<hostid>.*` queue protocol described below - the backend already models
Host/Cluster with a `hypervisor` discriminator and the messages are
hypervisor-agnostic.

Stack: Go (Windows only)
Message queue: RabbitMQ
Backend communication: exclusively via RabbitMQ
Build: `./build.sh` (mac/Linux) or `.\build.ps1` (Windows) → `ovc-agent.exe` at the repo root.
Version: `AgentVersion` constant in `internal/tasks/agent_status.go` (single source).
Local job persistence: SQLite (`modernc.org/sqlite`, pure Go)

Contract source of truth: `../ovc-backend/app/messaging/` (protocol.py, queues.py)
and `../ovc-backend/app/services/inventory.py`.

## Conventions

All code, comments, identifiers, commit messages, docs and log strings are in
**English**, regardless of the language a contributor chats in.

## RabbitMQ queues (per host)

- `<hostid>.request` - backend publishes `AgentRequest`, the agent consumes
- `<hostid>.response` - the agent publishes `AgentResponse`, the backend consumes
- `<hostid>.agent_status` - last-value, the agent publishes periodically
- `<hostid>.vm_inventory` - last-value
- `<hostid>.host_inventory` - last-value (hardware)
- `<hostid>.template_inventory` - last-value
- `<hostid>.iso_inventory` - last-value

Last-value queues: `x-max-length=1`, `x-overflow=drop-head`.

The worker subprocess publishes `response` and the state queues via
`Publisher.PublishConfirmed` (publisher confirms + `mandatory`): it blocks until
the broker `ack`s, and otherwise logs `NOT confirmed by broker` with the reason
(nack / unroutable / timeout). Needed because the worker calls `os.Exit` a few ms
after publishing - an unconfirmed publish is lost on a slow link. Progress
updates stay fire-and-forget (`PublishTo`).

## Message envelope

`AgentRequest`: `{ id, function, params, requested_by, issued_at }`
`AgentResponse`: `{ id, function, status, progress, result, error, vm_id, vm_state, finished_at }`
`status` ∈ `running | succeeded | failed`.

## Flow

The agent listens on `<hostid>.request`. On receipt it validates that the
`function` exists, persists a job in SQLite and spawns a worker subprocess
(`ovc-agent.exe job <id>`) to run it. The `jobqueue` controls concurrency (reads
are free, writes are mutually exclusive per VM, a host-write blocks everything).

### VM functions (module `vm_management`)

`vm_create`, `vm_edit`, `vm_shutdown`, `vm_start`, `vm_stop`, `vm_restart`,
`vm_delete`, `vm_inventory` (+ snapshots, migration, dvd, HA, export template,
rename, move, clone, notes, batch start/stop).

**`vm_export_template`** exports the VM, then converts every Fixed `.vhd`/`.vhdx`
in the exported folder to Dynamic in place (same filename kept, so the `.vmcx`
stays valid) to shrink the template store - skipped if the export contains
checkpoint/`.avhdx` disks, best-effort/non-fatal per disk. It then writes
`ovc-template-metadata.json` (stable UUID, name, `notes` from the params, source
VM, `exported_at` timestamp, host, and `size` = provisioned disk size).
`template_inventory` reads that JSON and reports `sizeBytes` (provisioned, from
`Get-OvcTemplateSize` or the metadata `size`), `diskSizeBytes` (real on-disk
folder footprint, measured natively in Go), `notes`, `createdAt` (the metadata
`exported_at`), and `cpuCount` / `memoryMb` (from `Compare-VM` on the `.vmcx`).

**`vm_clone`** (`clone_type` `imported` = clone an off VM, `exported` = deploy an
exported template folder) resolves its own destination folder from
`destination_storage` (same `resolveVMFolder` rules as `vm_create`) and, for
`imported`, its own scratch export folder - the backend sends neither `path` nor
`export_folder_root`. For `exported`, the deployed VM's Dynamic disks are
converted to Fixed after import by default (best-effort/non-fatal per disk,
skipped when the full size would not fit); `expand_disks: false` keeps them Dynamic.

### host functions (module `host_management`)

`host_hwinventory`, `host_update_agent` (agent upgrade), `host_restart_agent`
(+ cluster operations: suspend / resume / restart).

Artifact download now boils down to the **agent upgrade** (`host_update_agent`):
it downloads the new binary, verifies the checksum, enters drain mode and swaps
itself in.

## Periodic publications

`agent_status`, `vm_inventory`, `host_hwinventory` (-> host_inventory),
`template_inventory`, `iso_inventory`. Configurable intervals
(`refresh_interval_vms`, `refresh_interval_host`).

`agent_status` is the liveness heartbeat: it runs on its own short interval
(`heartbeat_interval`, default 60s), NOT on `refresh_interval_host`. The backend
marks the host offline if the last `agent_status` is older than
`agent_offline_after_seconds` (120s), so the heartbeat must stay well below that
even while the heavy host inventory runs every 10 min.

`host_hwinventory`: the collector assembles the rich `HardwareInventoryResult`
struct (PowerShell + native Go), but its `MarshalJSON` (`marshal_hardware.go`)
reshapes it into the canonical form the backend expects -
`cpu{model,sockets,cores,logical}`, `memoryBytes`,
`storage[]{path,label,totalBytes,freeBytes}`, `os`, `network:[]`,

- `system`/`bootTime`/`load`/`cluster`/`hyperv`. `totalVMs`/`uptimeDays`/
  `autoBalancer*` are collected but not sent.

The `Scheduler` creates internal jobs with `TaskID` `refresh-<kind>-<ts>`
(`jobqueue.PeriodicRefreshTaskID` / `IsPeriodicRefreshTaskID`). Since **no backend
task** is waiting on them, the worker publishes only to the state queue
(`<hostid>.<kind>`) - it does **not** send an `AgentResponse` on `<hostid>.response`.

`host_metrics` + `vm_metrics` - quick metrics (CPU / memory / disk / network),
sampled every `metrics_interval` seconds (default 300, `0` disables). Plain
durable queues (NOT last-value): a short time-series, the backend keeps only the
last hour. `vm_metrics` only includes VMs with Hyper-V resource metering enabled
(`Enable-VMResourceMetering` via the `vm_enable_metrics` action); `vm_inventory`
reports `metricsEnabled` per VM.

`agent_status` also reports the agent type (`hypervisor: "hyperv"`,
`agent_type: "ovc-agent-hyperv"`).

## Startup

Checks that the Hyper-V role is installed. If not, it writes `agent_failed.log`
in the binary's folder and exits.

## Config

The agent has NO fixed install folder: the binary's folder is the default folder
(`config.ini`, `agent.log`, `jobs.db`, and upgrade binaries all live there).
Config is read ONLY from `config.ini` (no registry, no env). Example:

```ini
[agent]
host_id = <id generated by the backend when the host is registered>
rabbitmq_url = amqp://user:password@rabbit:5672/
log_level = info
refresh_interval_vms = 180
refresh_interval_host = 600
heartbeat_interval = 60
metrics_interval = 300
template_path = D:\HyperV\TEMPLATES
local_iso_path = D:\HyperV\ISOS
aditional_vm_storage = E:\HyperV
```
