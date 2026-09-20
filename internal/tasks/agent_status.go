package tasks

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"time"

	"ovc-agent/internal/hyperv"
	"ovc-agent/internal/protocol"
)

const AgentVersion = "1.0.5"

// Hypervisor / AgentType identify this build in the agent_status payload so the
// backend can set Host.hypervisor (see ovc-backend LLM.md).
const (
	Hypervisor = "hyperv"
	AgentType  = "ovc-agent-hyperv"
)

// AgentStatusResult holds the agent status response.
type AgentStatusResult struct {
	AgentVersion        string `json:"agent_version"`
	Hostname            string `json:"hostname"`
	FQDN                string `json:"fqdn"`
	IPAddress           string `json:"ip"`
	UptimeSeconds       int64  `json:"uptime_seconds"`
	OSVersion           string `json:"os_version"`
	Hypervisor          string `json:"hypervisor"`
	AgentType           string `json:"agent_type"`
	VMRefreshInterval   int    `json:"vm_refresh_interval"`
	HostRefreshInterval int    `json:"host_refresh_interval"`
	ServiceIsDomainUser *bool  `json:"service_is_domain_user,omitempty"`
}

// toPayload maps the result onto the backend's AgentStatusPayload shape
// (app/messaging/protocol.py + services/inventory.py::apply_agent_status).
func (r *AgentStatusResult) toPayload(now string) protocol.AgentStatusPayload {
	return protocol.AgentStatusPayload{
		Version:             r.AgentVersion,
		VMRefreshInterval:   r.VMRefreshInterval,
		HostRefreshInterval: r.HostRefreshInterval,
		Hypervisor:          r.Hypervisor,
		AgentType:           r.AgentType,
		Hostname:            r.Hostname,
		FQDN:                r.FQDN,
		IP:                  r.IPAddress,
		OSVersion:           r.OSVersion,
		UptimeSeconds:       r.UptimeSeconds,
		ReportedAt:          now,
	}
}

func (d *Dispatcher) handleAgentStatus(ctx context.Context) (*AgentStatusResult, error) {
	hn, _ := os.Hostname()
	uptime := int64(time.Since(d.startTime).Seconds())

	osVersion, err := hyperv.RunPowerShellRaw(ctx, `(Get-CimInstance Win32_OperatingSystem).Caption`)
	if err != nil {
		osVersion = "unknown"
	}

	ip, fqdn := d.resolvePrimaryAddress(ctx)
	if fqdn == "" {
		fqdn = hn
	}

	result := &AgentStatusResult{
		AgentVersion:        AgentVersion,
		Hostname:            hn,
		FQDN:                fqdn,
		IPAddress:           ip,
		UptimeSeconds:       uptime,
		OSVersion:           strings.TrimSpace(osVersion),
		Hypervisor:          Hypervisor,
		AgentType:           AgentType,
		VMRefreshInterval:   d.vmRefreshInterval,
		HostRefreshInterval: d.hostRefreshInterval,
	}

	// If this node is part of a cluster, check whether the service runs under a domain account.
	if d.cluster.IsClusterNode {
		isDomain := d.checkServiceIsDomainUser(ctx)
		result.ServiceIsDomainUser = &isDomain
	}

	return result, nil
}

// resolvePrimaryAddress returns the host's management/primary IPv4 and FQDN.
//
// "Primary" = the interface used to reach the site network, never a dedicated
// vMotion / live-migration / storage link. A Hyper-V host is not assumed to have
// Internet access, so we do NOT probe a public address. Instead:
//
//  1. PowerShell ranks the Up adapters that hold an IPv4 address by
//     (has default gateway) then (has DNS servers) then (lowest route+iface
//     metric) and returns the winner's IPv4 - plus the configured DNS servers.
//  2. Go route-lookups each DNS server (a connected UDP socket sends nothing,
//     it only consults the routing table): the source address the OS would use
//     to reach its own DNS server is, by construction, the management IP.
//  3. If both fail, the first non-loopback / non-APIPA IPv4 on an up interface.
//
// The DNS-based lookup (step 2) is preferred - DNS is a service the host must be
// able to reach, and it distinguishes the management NIC even on hosts with
// several routed interfaces.
func (d *Dispatcher) resolvePrimaryAddress(ctx context.Context) (ip, fqdn string) {
	const script = `
$ErrorActionPreference = 'SilentlyContinue'

$cfgs = @(Get-NetIPConfiguration | Where-Object { $_.NetAdapter.Status -eq 'Up' -and $_.IPv4Address })
# Sort-Object is stable; chain least-significant key first so the final order is
# (has default gateway) then (has IPv4 DNS servers) then (lowest route+iface metric).
$best = $cfgs |
    Sort-Object { [int]($_.IPv4DefaultGateway.RouteMetric | Measure-Object -Minimum).Minimum + [int]$_.NetIPv4Interface.InterfaceMetric } |
    Sort-Object { if (@($_.DNSServer | Where-Object { $_.AddressFamily -eq 2 -and $_.ServerAddresses })) { 0 } else { 1 } } |
    Sort-Object { if ($_.IPv4DefaultGateway) { 0 } else { 1 } } |
    Select-Object -First 1
$ip = ($best.IPv4Address | Where-Object { $_.IPAddress -notlike '169.254.*' } | Select-Object -First 1).IPAddress

$dns = @(Get-DnsClientServerAddress -AddressFamily IPv4 |
    ForEach-Object { $_.ServerAddresses } |
    Where-Object { $_ -and $_ -notlike '127.*' -and $_ -notlike '169.254.*' } |
    Select-Object -Unique)

$fqdn = ""
try { $fqdn = [System.Net.Dns]::GetHostEntry($env:COMPUTERNAME).HostName } catch {}
if (-not $fqdn) {
    $cs = Get-CimInstance Win32_ComputerSystem
    if ($cs.Domain -and $cs.Domain -ne 'WORKGROUP') { $fqdn = "$($cs.Name).$($cs.Domain)" }
    else { $fqdn = $cs.Name }
}

[pscustomobject]@{ ip = "$ip"; fqdn = "$fqdn"; dns = $dns } | ConvertTo-Json -Compress
`
	var dnsServers []string
	if out, err := hyperv.RunPowerShell(ctx, script); err == nil {
		var parsed struct {
			IP   string          `json:"ip"`
			FQDN string          `json:"fqdn"`
			DNS  json.RawMessage `json:"dns"`
		}
		if json.Unmarshal(trimPowerShellOutput(out), &parsed) == nil {
			ip = strings.TrimSpace(parsed.IP)
			fqdn = strings.TrimSpace(parsed.FQDN)
			dnsServers = decodeStringArrayOrScalar(parsed.DNS)
		}
	} else {
		d.logger.Warn("primary address query failed", "error", err)
	}

	// Prefer the interface that routes to the host's own DNS server.
	if viaDNS := sourceIPForAny(dnsServers); viaDNS != "" {
		ip = viaDNS
	}
	if ip == "" {
		ip = firstUsableIPv4()
	}
	return ip, fqdn
}

// sourceIPForAny returns the local IPv4 the kernel would use to reach the first
// of the given hosts. A connected UDP socket performs no I/O - it just resolves
// the route - so this works on an air-gapped host as long as a route exists.
func sourceIPForAny(hosts []string) string {
	for _, h := range hosts {
		c, err := net.Dial("udp", net.JoinHostPort(h, "53"))
		if err != nil {
			continue
		}
		a, ok := c.LocalAddr().(*net.UDPAddr)
		c.Close()
		if ok && a.IP != nil {
			v4 := a.IP.To4()
			if v4 != nil && !v4.IsLoopback() && !v4.IsLinkLocalUnicast() && !v4.IsUnspecified() {
				return v4.String()
			}
		}
	}
	return ""
}

// firstUsableIPv4 is the last-resort local address: the first global-unicast
// IPv4 on an up, non-loopback interface. No network access required.
func firstUsableIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if v4 := ipn.IP.To4(); v4 != nil && v4.IsGlobalUnicast() && !v4.IsLinkLocalUnicast() {
				return v4.String()
			}
		}
	}
	return ""
}

// decodeStringArrayOrScalar handles a PowerShell ConvertTo-Json field that is an
// array, a bare string (single element), or absent.
func decodeStringArrayOrScalar(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var one string
	if json.Unmarshal(raw, &one) == nil && one != "" {
		return []string{one}
	}
	return nil
}

// checkServiceIsDomainUser queries the ovc-agent service's StartName (logon
// account) and returns true if it's a domain user (DOMAIN\user or user@domain)
// rather than a local account (LocalSystem, NT AUTHORITY\*, etc.).
func (d *Dispatcher) checkServiceIsDomainUser(ctx context.Context) bool {
	script := `(Get-CimInstance Win32_Service -Filter "Name='ovc-agent'").StartName`
	output, err := hyperv.RunPowerShellRaw(ctx, script)
	if err != nil {
		d.logger.Warn("failed to query service logon account", "error", err)
		return false
	}

	startName := strings.TrimSpace(output)
	if startName == "" {
		return false
	}

	upper := strings.ToUpper(startName)
	if upper == "LOCALSYSTEM" || upper == "LOCAL SYSTEM" {
		return false
	}
	if strings.HasPrefix(upper, "NT AUTHORITY\\") || strings.HasPrefix(upper, "NT SERVICE\\") {
		return false
	}
	if strings.HasPrefix(startName, `.\\`) || strings.HasPrefix(startName, `.\ `) {
		return false
	}
	if strings.Contains(startName, `\`) {
		parts := strings.SplitN(startName, `\`, 2)
		if strings.EqualFold(parts[0], hostname()) {
			return false
		}
		return true
	}
	if strings.Contains(startName, "@") {
		return true
	}
	return false
}

// hostname returns the computer name for comparison (best-effort).
func hostname() string {
	h, _ := os.Hostname()
	return h
}
