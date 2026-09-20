package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/ini.v1"
)

const (
	DefaultLogLevel            = "info"
	DefaultTaskTimeout         = 60
	DefaultRefreshIntervalVMs  = 300 // 5 minutes
	DefaultRefreshIntervalHost = 600 // 10 minutes
	DefaultMetricsInterval     = 300 // 5 minutes - quick host/VM metrics sampling
	DefaultMaxConcurrentJobs   = 4

	// DefaultHeartbeatInterval is how often the agent republishes agent_status.
	// It is independent of refresh_interval_host: the backend marks a host
	// offline when agent_status is older than agent_offline_after_seconds
	// (120s by default), so this must stay well below that regardless of how
	// slow the heavy host inventory runs. Never 0 - the heartbeat is what keeps
	// the host shown as online.
	DefaultHeartbeatInterval = 60 // 1 minute

	// ConfigFileName / LogFileName / JobStoreFileName live in AgentDir().
	ConfigFileName   = "config.ini"
	LogFileName      = "agent.log"
	JobStoreFileName = "jobs.db"
)

// AgentDir returns the directory the agent binary lives in. Every agent file
// (config.ini, agent.log, jobs.db, downloaded upgrade binaries) is resolved
// relative to it - the agent has no fixed install path. Falls back to the
// current working directory if the executable path cannot be determined.
func AgentDir() string {
	exe, err := os.Executable()
	if err != nil {
		if wd, wErr := os.Getwd(); wErr == nil {
			return wd
		}
		return "."
	}
	if resolved, rErr := filepath.EvalSymlinks(exe); rErr == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}

// DefaultConfigPath is <AgentDir>/config.ini.
func DefaultConfigPath() string { return filepath.Join(AgentDir(), ConfigFileName) }

// DefaultLogFile is <AgentDir>/agent.log.
func DefaultLogFile() string { return filepath.Join(AgentDir(), LogFileName) }

// JobStorePath is <AgentDir>/jobs.db.
func JobStorePath() string { return filepath.Join(AgentDir(), JobStoreFileName) }

// Config holds the agent configuration. It is read exclusively from config.ini
// in AgentDir() (or an explicit path passed to Load).
type Config struct {
	HostID              string
	RabbitMQURL         string
	LogLevel            string
	LogFile             string
	TaskTimeout         int
	MaxConcurrentJobs   int      // max number of concurrent worker processes (1-64, default 4)
	RefreshIntervalVMs  int      // seconds between automatic VM inventory pushes (0 = disabled)
	RefreshIntervalHost int      // seconds between automatic host inventory pushes (0 = disabled)
	MetricsInterval     int      // seconds between quick host/VM metrics samples (0 = disabled)
	HeartbeatInterval   int      // seconds between agent_status heartbeats (always on; <=0 falls back to default)
	TemplatePath        string   // path where exported VM templates are stored
	LocalISOPath        string   // path where ISO files are stored (optional)
	AdditionalVMStorage []string // extra VM storage roots (normalized to end with "HyperV")
}

func newConfig() *Config {
	return &Config{
		LogLevel:            DefaultLogLevel,
		LogFile:             DefaultLogFile(),
		TaskTimeout:         DefaultTaskTimeout,
		MaxConcurrentJobs:   DefaultMaxConcurrentJobs,
		RefreshIntervalVMs:  DefaultRefreshIntervalVMs,
		RefreshIntervalHost: DefaultRefreshIntervalHost,
		MetricsInterval:     DefaultMetricsInterval,
		HeartbeatInterval:   DefaultHeartbeatInterval,
	}
}

// Load reads configuration from config.ini. An empty configPath means
// <AgentDir>/config.ini.
func Load(configPath string) (*Config, error) {
	if configPath == "" {
		configPath = DefaultConfigPath()
	}

	cfg := newConfig()
	if err := cfg.loadFromINI(configPath); err != nil {
		return nil, err
	}

	// Validate required fields
	if cfg.HostID == "" {
		return nil, fmt.Errorf("host_id is required but not configured in %s", configPath)
	}
	if cfg.RabbitMQURL == "" {
		return nil, fmt.Errorf("rabbitmq_url is required but not configured in %s", configPath)
	}
	if cfg.TemplatePath == "" {
		return nil, fmt.Errorf("template_path is required but not configured in %s", configPath)
	}
	if cfg.LocalISOPath == "" {
		return nil, fmt.Errorf("local_iso_path is required but not configured in %s", configPath)
	}

	// Validate configured filesystem paths exist on the OS. Any missing path is fatal.
	if err := cfg.validatePaths(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// LoadForMessaging loads only the fields required to publish a message to RabbitMQ
// (HostID and RabbitMQURL), skipping the strict filesystem-path validation performed
// by Load. It is used by the upgrade process to report a failure response even when
// the full config is invalid (e.g. a bad template_path), which is often the very
// reason the upgraded agent failed to start.
func LoadForMessaging(configPath string) (*Config, error) {
	if configPath == "" {
		configPath = DefaultConfigPath()
	}

	cfg := newConfig()
	if err := cfg.loadFromINI(configPath); err != nil {
		return nil, err
	}

	if cfg.HostID == "" {
		return nil, fmt.Errorf("host_id is required but not configured in %s", configPath)
	}
	if cfg.RabbitMQURL == "" {
		return nil, fmt.Errorf("rabbitmq_url is required but not configured in %s", configPath)
	}

	return cfg, nil
}

// dirExists returns true if the path exists and is a directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// validatePaths ensures all configured storage paths exist on the OS.
// It validates template_path, local_iso_path, and every additional VM storage path.
func (c *Config) validatePaths() error {
	if !dirExists(c.TemplatePath) {
		return fmt.Errorf("template_path does not exist or is not a directory: %s", c.TemplatePath)
	}
	if !dirExists(c.LocalISOPath) {
		return fmt.Errorf("local_iso_path does not exist or is not a directory: %s", c.LocalISOPath)
	}
	for _, p := range c.AdditionalVMStorage {
		// The "HyperV" leaf may be created by the agent on demand, so validate that
		// its parent (the configured storage root / volume) exists on the OS.
		base := filepath.Dir(p)
		if !dirExists(p) && !dirExists(base) {
			return fmt.Errorf("aditional_vm_storage path does not exist or is not a directory: %s", p)
		}
	}
	return nil
}

func (c *Config) loadFromINI(path string) error {
	f, err := ini.Load(path)
	if err != nil {
		return fmt.Errorf("cannot read config file %s: %w", path, err)
	}

	section := f.Section("agent")

	if v := section.Key("host_id").String(); v != "" {
		c.HostID = v
	}
	if v := section.Key("rabbitmq_url").String(); v != "" {
		c.RabbitMQURL = v
	}
	if v := section.Key("log_level").String(); v != "" {
		c.LogLevel = strings.ToLower(v)
	}
	if v := section.Key("log_file").String(); v != "" {
		c.LogFile = v
	}
	if v, err := section.Key("task_timeout").Int(); err == nil && v > 0 {
		c.TaskTimeout = v
	}
	if v, err := section.Key("refresh_interval_vms").Int(); err == nil && v >= 0 {
		c.RefreshIntervalVMs = v
	}
	if v, err := section.Key("refresh_interval_host").Int(); err == nil && v >= 0 {
		c.RefreshIntervalHost = v
	}
	if v, err := section.Key("metrics_interval").Int(); err == nil && v >= 0 {
		c.MetricsInterval = v
	}
	if v, err := section.Key("heartbeat_interval").Int(); err == nil && v > 0 {
		c.HeartbeatInterval = v
	}
	if v := section.Key("template_path").String(); v != "" {
		c.TemplatePath = v
	}
	if v := section.Key("local_iso_path").String(); v != "" {
		c.LocalISOPath = v
	}
	if v := section.Key("aditional_vm_storage").String(); v != "" {
		c.AdditionalVMStorage = parseAdditionalStorage(v)
	}
	if v, err := section.Key("max_concurrent_jobs").Int(); err == nil {
		if v >= 1 && v <= 64 {
			c.MaxConcurrentJobs = v
		} else {
			slog.Warn("max_concurrent_jobs out of range (1-64), using default", "value", v, "default", DefaultMaxConcurrentJobs)
		}
	}

	return nil
}

// Queue names follow the ovc-backend layout (app/messaging/queues.py):
// "<hostid>.<kind>" for every per-host queue.

// RequestQueue is the queue the agent consumes requests from (backend -> agent).
func (c *Config) RequestQueue() string {
	return fmt.Sprintf("%s.request", c.HostID)
}

// ResponseQueue is the queue the agent publishes task responses to (agent -> backend).
func (c *Config) ResponseQueue() string {
	return fmt.Sprintf("%s.response", c.HostID)
}

// StateQueue returns the last-value queue for a given inventory kind
// (agent_status | vm_inventory | host_inventory | template_inventory | iso_inventory).
func (c *Config) StateQueue(kind string) string {
	return fmt.Sprintf("%s.%s", c.HostID, kind)
}
