// Package agent defines the Worker's own configuration (as opposed to the
// service configs it orchestrates). The agent config is pushed out-of-band to
// every node (mounted file or env) and governs the command-execution policy:
// a blacklist/whitelist of allowed commands, timeouts, and capability
// switches for host execution and container exec.
package agent

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Role of this worker instance.
type Role string

const (
	// RoleAuto derives the role from the swarm node type (manager vs worker).
	RoleAuto Role = "auto"
	// RoleManager runs orchestration + aggregation + MCP.
	RoleManager Role = "manager"
	// RoleNode runs only local collection/exec and reports to the manager.
	RoleNode Role = "node"
)

// Config is the agent's own configuration file.
type Config struct {
	Worker        WorkerConfig  `yaml:"worker" json:"worker"`
	CommandPolicy CommandPolicy `yaml:"commandPolicy" json:"commandPolicy"`
	// Webhooks is DEPRECATED. Monitoring events are now streamed to the server
	// over the gRPC SubscribeEvents bidirectional stream. The field is kept for
	// backward compatibility with old config files and is ignored.
	Webhooks []string `yaml:"webhooks" json:"webhooks,omitempty"`
	Auth     AuthConfig `yaml:"auth" json:"auth"`
}

// AuthConfig controls access to the HTTP API and MCP endpoint.
type AuthConfig struct {
	// Enabled turns on bearer-token auth (both /api/* and /mcp).
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Tokens maps a token name -> secret. The name is used as the audit actor.
	Tokens map[string]string `yaml:"tokens" json:"tokens,omitempty"`
}

// WorkerConfig identifies this instance's role and connectivity.
type WorkerConfig struct {
	Role string `yaml:"role" json:"role"` // auto | manager | node
	// Listen is the HTTP listen address of the local node API (and, on
	// managers, the /mcp + /healthz endpoints). Default :8080.
	Listen string `yaml:"listen" json:"listen"` // default :8080
	// GrpcListen is the gRPC listen address for the management API
	// (server↔worker), manager-role only. Default :9080. Overridable by the
	// -grpc-addr flag.
	GrpcListen string `yaml:"grpcListen" json:"grpcListen,omitempty"`
	// DataDir is the directory for persistent state (event/audit SQLite
	// queues). Default /var/lib/opsguard. Overridable by the -data-dir flag.
	DataDir string `yaml:"dataDir" json:"dataDir,omitempty"`
	// ManagerURL is the manager-role worker base URL; node-role workers use it
	// to know where to report/proxy. Optional in v1 (manager pulls node data).
	ManagerURL string `yaml:"managerURL" json:"managerURL,omitempty"`
}

// CommandPolicy governs command execution (host and container).
type CommandPolicy struct {
	// Mode is "blacklist" (reject dangerous commands, allow the rest) or
	// "whitelist" (allow only listed commands).
	Mode string `yaml:"mode" json:"mode"`
	// AllowHostExec enables executing commands on the host (nsenter).
	AllowHostExec bool `yaml:"allowHostExec" json:"allowHostExec"`
	// AllowContainerExec enables executing commands inside containers.
	AllowContainerExec bool `yaml:"allowContainerExec" json:"allowContainerExec"`
	// Timeout bounds a single command execution.
	Timeout string `yaml:"timeout" json:"timeout"`
	// Blacklist substrings: any command containing one is rejected.
	Blacklist []string `yaml:"blacklist" json:"blacklist"`
	// Whitelist command prefixes: in whitelist mode the command name (and for
	// docker/systemctl, the subcommand) must match one of these.
	Whitelist []string `yaml:"whitelist" json:"whitelist"`
}

// DefaultConfig returns a safe-by-default policy: blacklist mode with common
// destructive commands blocked, host exec disabled until explicitly enabled.
func DefaultConfig() *Config {
	return &Config{
		Worker: WorkerConfig{
			Role:   string(RoleAuto),
			Listen: ":8080",
		},
		CommandPolicy: CommandPolicy{
			Mode:               "blacklist",
			AllowHostExec:      false,
			AllowContainerExec: true,
			Timeout:            "30s",
			Blacklist:          DefaultBlacklist,
		},
	}
}

// DefaultBlacklist is the built-in set of dangerous command substrings. It is
// merged with any user-supplied blacklist entries.
var DefaultBlacklist = []string{
	"rm -rf /",
	"rm -fr /",
	"rm -rf /*",
	"shutdown",
	"reboot",
	"poweroff",
	"halt",
	"mkfs",
	"dd if=/dev/zero",
	"dd if=/dev/random of=/dev/sd",
	"of=/dev/sda",
	"of=/dev/sdb",
	":(){ :|:& };:",
	"chmod -R 777 /",
	"chown -R 0:0 /",
	"curl http://|sh",
	"wget http://|sh",
	"curl |bash",
	"bash -c \"rm",
	"init 0",
	"init 6",
}

// Load reads the agent config from a YAML file, applying defaults for missing
// fields and validating it, then applies environment overrides (for
// centralized distribution via orchestration platforms / config maps).
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read agent config: %w", err)
	}
	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse agent config: %w", err)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.ApplyEnvOverrides()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("after env overrides: %w", err)
	}
	return cfg, nil
}

// ApplyEnvOverrides lets an orchestrator (swarm stack environment, K8s
// configmap, ...) centrally distribute the critical policy knobs without
// touching the per-node file:
//
//	WORKER_ROLE            worker.role
//	WORKER_ALLOW_HOST_EXEC allowHostExec (true/false)
//	WORKER_ALLOW_CONTAINER allowContainerExec (true/false)
//	WORKER_COMMAND_TIMEOUT commandPolicy.timeout
//	WORKER_WEBHOOKS        comma-separated webhook URLs
//	WORKER_TOKENS          "name=secret,name2=secret2" (auth tokens)
func (c *Config) ApplyEnvOverrides() {
	if v := os.Getenv("WORKER_ROLE"); v != "" {
		c.Worker.Role = v
	}
	if v := os.Getenv("WORKER_ALLOW_HOST_EXEC"); v != "" {
		c.CommandPolicy.AllowHostExec = v == "true" || v == "1"
	}
	if v := os.Getenv("WORKER_ALLOW_CONTAINER"); v != "" {
		c.CommandPolicy.AllowContainerExec = v == "true" || v == "1"
	}
	if v := os.Getenv("WORKER_COMMAND_TIMEOUT"); v != "" {
		c.CommandPolicy.Timeout = v
	}
	if v := os.Getenv("WORKER_WEBHOOKS"); v != "" {
		c.Webhooks = splitCSV(v)
	}
	if v := os.Getenv("WORKER_TOKENS"); v != "" {
		c.Auth.Enabled = true
		if c.Auth.Tokens == nil {
			c.Auth.Tokens = map[string]string{}
		}
		for _, pair := range strings.Split(v, ",") {
			if k, val, ok := strings.Cut(pair, "="); ok && k != "" && val != "" {
				c.Auth.Tokens[strings.TrimSpace(k)] = strings.TrimSpace(val)
			}
		}
	}
}

func splitCSV(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ApplyDefaults fills zero values.
func (c *Config) ApplyDefaults() {
	if c.Worker.Role == "" {
		c.Worker.Role = string(RoleAuto)
	}
	if c.Worker.Listen == "" {
		c.Worker.Listen = ":8080"
	}
	if c.CommandPolicy.Mode == "" {
		c.CommandPolicy.Mode = "blacklist"
	}
	if c.CommandPolicy.Timeout == "" {
		c.CommandPolicy.Timeout = "30s"
	}
	if c.CommandPolicy.Mode == "blacklist" {
		// merge built-in dangerous commands, dedup
		seen := map[string]bool{}
		for _, b := range c.CommandPolicy.Blacklist {
			seen[b] = true
		}
		for _, d := range DefaultBlacklist {
			if !seen[d] {
				c.CommandPolicy.Blacklist = append(c.CommandPolicy.Blacklist, d)
			}
		}
	}
}

// Validate checks the config for semantic errors.
func (c *Config) Validate() error {
	switch Role(c.Worker.Role) {
	case RoleAuto, RoleManager, RoleNode:
	default:
		return fmt.Errorf("worker.role %q must be auto|manager|node", c.Worker.Role)
	}
	switch c.CommandPolicy.Mode {
	case "blacklist", "whitelist":
	default:
		return fmt.Errorf("commandPolicy.mode %q must be blacklist|whitelist", c.CommandPolicy.Mode)
	}
	if _, err := time.ParseDuration(c.CommandPolicy.Timeout); err != nil {
		return fmt.Errorf("commandPolicy.timeout: %w", err)
	}
	return nil
}

// TimeoutDuration returns the parsed timeout.
func (p *CommandPolicy) TimeoutDuration() time.Duration {
	d, err := time.ParseDuration(p.Timeout)
	if err != nil || d <= 0 {
		return 30 * time.Second
	}
	return d
}
