// Package config defines the configuration contract pushed by the management
// side (OpsGaurdWeb) to a Worker. A single Config drives both orchestration
// (the Service block, mapped to a swarm.ServiceSpec by the orchestrator) and
// monitoring (the Monitoring block, consumed by the monitor).
//
// The schema is documented in docs/Worker-设计方案.md §4.2.
package config

// Config is the root configuration object.
type Config struct {
	Service    Service    `yaml:"service" json:"service"`
	Monitoring Monitoring `yaml:"monitoring" json:"monitoring"`
}

// Service describes the desired swarm service.
type Service struct {
	Name            string            `yaml:"name" json:"name"`
	Image           string            `yaml:"image" json:"image"`
	Mode            string            `yaml:"mode,omitempty" json:"mode,omitempty"`                       // replicated | global
	Replicas        *uint64           `yaml:"replicas,omitempty" json:"replicas,omitempty"`               // replicated only
	ImagePullPolicy string            `yaml:"imagePullPolicy,omitempty" json:"imagePullPolicy,omitempty"` // always | missing | never
	RegistryAuth    *RegistryAuth     `yaml:"registryAuth,omitempty" json:"registryAuth,omitempty"`
	Networks        []string          `yaml:"networks,omitempty" json:"networks,omitempty"`
	Ports           []PortConfig      `yaml:"ports,omitempty" json:"ports,omitempty"`
	Env             []string          `yaml:"env,omitempty" json:"env,omitempty"`
	Command         []string          `yaml:"command,omitempty" json:"command,omitempty"`
	Args            []string          `yaml:"args,omitempty" json:"args,omitempty"`
	Workdir         string            `yaml:"workdir,omitempty" json:"workdir,omitempty"`
	User            string            `yaml:"user,omitempty" json:"user,omitempty"`
	Mounts          []MountConfig     `yaml:"mounts,omitempty" json:"mounts,omitempty"`
	Secrets         []string          `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	Configs         []string          `yaml:"configs,omitempty" json:"configs,omitempty"`
	Labels          map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
	Resources       *Resources        `yaml:"resources,omitempty" json:"resources,omitempty"`
	Healthcheck     *Healthcheck      `yaml:"healthcheck,omitempty" json:"healthcheck,omitempty"`
	Placement       *Placement        `yaml:"placement,omitempty" json:"placement,omitempty"`
	Update          *UpdateConfig     `yaml:"update,omitempty" json:"update,omitempty"`
	Rollback        *UpdateConfig     `yaml:"rollback,omitempty" json:"rollback,omitempty"`
	Restart         *RestartPolicy    `yaml:"restart,omitempty" json:"restart,omitempty"`
	LogDriver       *LogDriver        `yaml:"logDriver,omitempty" json:"logDriver,omitempty"`
}

// RegistryAuth references credentials for pulling from a private registry.
type RegistryAuth struct {
	SecretRef string `yaml:"secretRef,omitempty" json:"secretRef,omitempty"` // swarm secret name holding ~/.docker/config.json
	Inline    string `yaml:"inline,omitempty" json:"inline,omitempty"`       // base64 of ~/.docker/config.json (convenience, not for prod)
}

// PortConfig maps to swarm.EndpointSpec.Ports.
type PortConfig struct {
	Published uint32 `yaml:"published,omitempty" json:"published,omitempty"`
	Target    uint32 `yaml:"target" json:"target"`
	Protocol  string `yaml:"protocol,omitempty" json:"protocol,omitempty"` // tcp | udp | sctp
	Mode      string `yaml:"mode,omitempty" json:"mode,omitempty"`         // ingress | host
}

// MountConfig maps to mount.Mount.
type MountConfig struct {
	Type     string `yaml:"type" json:"type"` // volume | bind | tmpfs
	Source   string `yaml:"source,omitempty" json:"source,omitempty"`
	Target   string `yaml:"target" json:"target"`
	ReadOnly bool   `yaml:"readonly,omitempty" json:"readonly,omitempty"`
}

// Resources maps to swarm.ResourceRequirements.
type Resources struct {
	Limits       *ResourceQuantities `yaml:"limits,omitempty" json:"limits,omitempty"`
	Reservations *ResourceQuantities `yaml:"reservations,omitempty" json:"reservations,omitempty"`
}

// ResourceQuantities holds CPU/memory limits or reservations.
type ResourceQuantities struct {
	CPU    string `yaml:"cpu,omitempty" json:"cpu,omitempty"`       // "1.0" or "500m"
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty"` // "512Mi"
}

// Healthcheck maps to container.HealthConfig.
type Healthcheck struct {
	Test        []string `yaml:"test,omitempty" json:"test,omitempty"` // e.g. ["CMD-SHELL","curl -f ..."]
	Interval    string   `yaml:"interval,omitempty" json:"interval,omitempty"`
	Timeout     string   `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Retries     int      `yaml:"retries,omitempty" json:"retries,omitempty"`
	StartPeriod string   `yaml:"startPeriod,omitempty" json:"startPeriod,omitempty"`
}

// Placement maps to swarm.Placement.
type Placement struct {
	Constraints []string     `yaml:"constraints,omitempty" json:"constraints,omitempty"`
	Preferences []Preference `yaml:"preferences,omitempty" json:"preferences,omitempty"`
}

// Preference maps to swarm.PlacementPreference (spread only for now).
type Preference struct {
	Spread string `yaml:"spread,omitempty" json:"spread,omitempty"`
}

// UpdateConfig maps to swarm.UpdateConfig / RollbackConfig.
type UpdateConfig struct {
	Parallelism     uint64  `yaml:"parallelism,omitempty" json:"parallelism,omitempty"`
	Delay           string  `yaml:"delay,omitempty" json:"delay,omitempty"`
	FailureAction   string  `yaml:"failureAction,omitempty" json:"failureAction,omitempty"` // pause | continue | rollback
	Monitor         string  `yaml:"monitor,omitempty" json:"monitor,omitempty"`
	MaxFailureRatio float32 `yaml:"maxFailureRatio,omitempty" json:"maxFailureRatio,omitempty"`
}

// RestartPolicy maps to swarm.RestartPolicy.
type RestartPolicy struct {
	Condition   string `yaml:"condition,omitempty" json:"condition,omitempty"` // any | on-failure | none
	Delay       string `yaml:"delay,omitempty" json:"delay,omitempty"`
	MaxAttempts uint64 `yaml:"maxAttempts,omitempty" json:"maxAttempts,omitempty"`
	Window      string `yaml:"window,omitempty" json:"window,omitempty"`
}

// LogDriver maps to swarm.Driver (TaskTemplate.LogDriver).
type LogDriver struct {
	Name    string            `yaml:"name,omitempty" json:"name,omitempty"`
	Options map[string]string `yaml:"options,omitempty" json:"options,omitempty"`
}

// ---- Monitoring block (module ②) ----

// Monitoring drives the monitor subsystem.
type Monitoring struct {
	Enabled            bool                `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	PortChecks         []PortCheck         `yaml:"portChecks,omitempty" json:"portChecks,omitempty"`
	HTTPChecks         []HTTPCheck         `yaml:"httpChecks,omitempty" json:"httpChecks,omitempty"`
	LogChecks          []LogCheck          `yaml:"logChecks,omitempty" json:"logChecks,omitempty"`
	ResourceThresholds []ResourceThreshold `yaml:"resourceThresholds,omitempty" json:"resourceThresholds,omitempty"`
}

// PortCheck is a TCP connectivity probe.
type PortCheck struct {
	Port     string `yaml:"port" json:"port"` // published port to probe
	Protocol string `yaml:"protocol,omitempty" json:"protocol,omitempty"`
	Interval string `yaml:"interval,omitempty" json:"interval,omitempty"`
	Timeout  string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Retries  int    `yaml:"retries,omitempty" json:"retries,omitempty"`
}

// HTTPCheck is an HTTP liveness/readiness probe.
type HTTPCheck struct {
	URL            string            `yaml:"url" json:"url"`
	Method         string            `yaml:"method,omitempty" json:"method,omitempty"`
	Headers        map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	ExpectedStatus []int             `yaml:"expectedStatus,omitempty" json:"expectedStatus,omitempty"`
	ExpectedBody   string            `yaml:"expectedBody,omitempty" json:"expectedBody,omitempty"` // regex
	Interval       string            `yaml:"interval,omitempty" json:"interval,omitempty"`
	Timeout        string            `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// LogCheck matches log lines against patterns.
type LogCheck struct {
	Pattern string   `yaml:"pattern" json:"pattern"` // Go regex
	Level   string   `yaml:"level,omitempty" json:"level,omitempty"`
	Ignore  []string `yaml:"ignore,omitempty" json:"ignore,omitempty"`
	Action  string   `yaml:"action,omitempty" json:"action,omitempty"` // alert | restart
}

// ResourceThreshold triggers when a resource metric crosses a percentage threshold.
type ResourceThreshold struct {
	Metric    string `yaml:"metric" json:"metric"`                     // cpu | memory
	Threshold int    `yaml:"threshold" json:"threshold"`               // percent 0-100
	Action    string `yaml:"action,omitempty" json:"action,omitempty"` // alert | restart
}
