package docker

// This file defines request/response structs for the Docker Engine REST API
// (https://docs.docker.com/engine/api/). Only the subset of fields Worker uses
// is modeled; unknown fields in responses are ignored by encoding/json.
//
// JSON keys are PascalCase and durations are nanoseconds (int64), matching the
// Engine API wire format. The Translator (internal/orchestrator) produces the
// request structs; the monitor consumes the response structs.

// ---- Service create/update request body (POST /services/create) ----

// ServiceSpec is the request body for service create and the spec portion of
// service update.
type ServiceSpec struct {
	Name           string                    `json:"Name,omitempty"`
	Labels         map[string]string         `json:"Labels,omitempty"`
	TaskTemplate   TaskSpec                  `json:"TaskTemplate"`
	Mode           ServiceMode               `json:"Mode,omitempty"`
	UpdateConfig   *UpdateConfig             `json:"UpdateConfig,omitempty"`
	RollbackConfig *UpdateConfig             `json:"RollbackConfig,omitempty"`
	Networks       []NetworkAttachmentConfig `json:"Networks,omitempty"`
	EndpointSpec   *EndpointSpec             `json:"EndpointSpec,omitempty"`
}

// TaskSpec holds the per-task template.
type TaskSpec struct {
	ContainerSpec ContainerSpec         `json:"ContainerSpec"`
	Resources     *ResourceRequirements `json:"Resources,omitempty"`
	RestartPolicy *RestartPolicy        `json:"RestartPolicy,omitempty"`
	Placement     *Placement            `json:"Placement,omitempty"`
	LogDriver     *Driver               `json:"LogDriver,omitempty"`
	// ForceUpdate, when incremented, forces swarm to re-create the tasks
	// (used by Restart / docker service update --force).
	ForceUpdate uint64 `json:"ForceUpdate,omitempty"`
}

// ContainerSpec is the container definition inside a task.
type ContainerSpec struct {
	Image       string             `json:"Image,omitempty"`
	Env         []string           `json:"Env,omitempty"`
	Command     []string           `json:"Command,omitempty"`
	Args        []string           `json:"Args,omitempty"`
	Dir         string             `json:"Dir,omitempty"`
	User        string             `json:"User,omitempty"`
	Labels      map[string]string  `json:"Labels,omitempty"`
	Mounts      []Mount            `json:"Mounts,omitempty"`
	Healthcheck *HealthConfig      `json:"Healthcheck,omitempty"`
	Secrets     []*SecretReference `json:"Secrets,omitempty"`
	Configs     []*ConfigReference `json:"Configs,omitempty"`
}

// ServiceMode is replicated or global.
type ServiceMode struct {
	Replicated *ReplicatedService `json:"Replicated,omitempty"`
	Global     *GlobalService     `json:"Global,omitempty"`
}

// ReplicatedService carries the desired replica count.
type ReplicatedService struct {
	Replicas *uint64 `json:"Replicas,omitempty"`
}

// GlobalService is present (empty) when the service is global.
type GlobalService struct{}

// Mount maps to the Engine API mount object.
type Mount struct {
	Type     string `json:"Type"` // volume | bind | tmpfs
	Source   string `json:"Source,omitempty"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly,omitempty"`
}

// HealthConfig maps to the container healthcheck object.
type HealthConfig struct {
	Test        []string `json:"Test,omitempty"`
	Interval    int64    `json:"Interval,omitempty"` // nanoseconds
	Timeout     int64    `json:"Timeout,omitempty"`
	StartPeriod int64    `json:"StartPeriod,omitempty"`
	Retries     int      `json:"Retries,omitempty"`
}

// SecretReference / ConfigReference reference swarm secrets/configs.
type SecretReference struct {
	SecretName string                     `json:"SecretName,omitempty"`
	File       *SecretReferenceFileTarget `json:"File,omitempty"`
}

type SecretReferenceFileTarget struct {
	Name string `json:"Name,omitempty"`
	UID  string `json:"UID,omitempty"`
	GID  string `json:"GID,omitempty"`
	Mode uint32 `json:"Mode,omitempty"`
}

type ConfigReference struct {
	ConfigName string                     `json:"ConfigName,omitempty"`
	File       *ConfigReferenceFileTarget `json:"File,omitempty"`
}

type ConfigReferenceFileTarget struct {
	Name string `json:"Name,omitempty"`
	UID  string `json:"UID,omitempty"`
	GID  string `json:"GID,omitempty"`
	Mode uint32 `json:"Mode,omitempty"`
}

// ResourceRequirements holds limits and reservations.
type ResourceRequirements struct {
	Limits       *Resources `json:"Limits,omitempty"`
	Reservations *Resources `json:"Reservations,omitempty"`
}

// Resources is the CPU/memory quantities. NanoCPUs = CPUs × 1e9.
type Resources struct {
	NanoCPUs    int64 `json:"NanoCPUs,omitempty"`
	MemoryBytes int64 `json:"MemoryBytes,omitempty"`
}

// RestartPolicy maps to the swarm restart policy.
type RestartPolicy struct {
	Condition   string `json:"Condition,omitempty"` // any | on-failure | none
	Delay       int64  `json:"Delay,omitempty"`     // nanoseconds
	MaxAttempts int64  `json:"MaxAttempts,omitempty"`
	Window      int64  `json:"Window,omitempty"`
}

// Placement holds scheduling constraints and spread preferences.
type Placement struct {
	Constraints []string              `json:"Constraints,omitempty"`
	Preferences []PlacementPreference `json:"Preferences,omitempty"`
}

// PlacementPreference is a spread directive.
type PlacementPreference struct {
	Spread *SpreadOverTag `json:"Spread,omitempty"`
}

// SpreadOverTag spreads tasks across nodes matching a label descriptor.
type SpreadOverTag struct {
	SpreadDescriptor string `json:"SpreadDescriptor,omitempty"`
}

// Driver is a named log driver with options.
type Driver struct {
	Name    string            `json:"Name,omitempty"`
	Options map[string]string `json:"Options,omitempty"`
}

// UpdateConfig governs rolling updates and rollbacks.
type UpdateConfig struct {
	Parallelism     uint64  `json:"Parallelism,omitempty"`
	Delay           int64   `json:"Delay,omitempty"` // nanoseconds
	FailureAction   string  `json:"FailureAction,omitempty"`
	Monitor         int64   `json:"Monitor,omitempty"` // nanoseconds
	MaxFailureRatio float32 `json:"MaxFailureRatio,omitempty"`
}

// NetworkAttachmentConfig references an attachable network.
type NetworkAttachmentConfig struct {
	Target string `json:"Target,omitempty"`
}

// EndpointSpec holds published ports.
type EndpointSpec struct {
	Ports []PortConfig `json:"Ports,omitempty"`
}

// PortConfig is a published/target port mapping.
type PortConfig struct {
	Name          string `json:"Name,omitempty"`
	Protocol      string `json:"Protocol,omitempty"` // tcp | udp | sctp
	TargetPort    uint32 `json:"TargetPort"`
	PublishedPort uint32 `json:"PublishedPort,omitempty"`
	PublishMode   string `json:"PublishMode,omitempty"` // ingress | host
}

// ---- Response types ----

// Version is the optimistic-concurrency version returned with mutable objects.
type Version struct {
	Index uint64 `json:"Index"`
}

// Service is the service object returned by inspect/list.
type Service struct {
	ID            string        `json:"ID"`
	Version       Version       `json:"Version"`
	CreatedAt     string        `json:"CreatedAt,omitempty"`
	UpdatedAt     string        `json:"UpdatedAt,omitempty"`
	Spec          ServiceSpec   `json:"Spec"`
	ServiceStatus ServiceStatus `json:"ServiceStatus,omitempty"`
	Endpoint      Endpoint      `json:"Endpoint,omitempty"`
}

// ServiceStatus is present when services are listed with status=true.
type ServiceStatus struct {
	RunningTasks   uint64 `json:"RunningTasks"`
	DesiredTasks   uint64 `json:"DesiredTasks"`
	CompletedTasks uint64 `json:"CompletedTasks,omitempty"`
}

// Endpoint holds the resolved service endpoint.
type Endpoint struct {
	Spec  EndpointSpec `json:"Spec,omitempty"`
	Ports []PortConfig `json:"Ports,omitempty"`
}

// Task is a single scheduled unit of a service.
type Task struct {
	ID           string     `json:"ID"`
	Version      Version    `json:"Version"`
	CreatedAt    string     `json:"CreatedAt,omitempty"`
	ServiceID    string     `json:"ServiceID,omitempty"`
	Slot         int        `json:"Slot,omitempty"`
	NodeID       string     `json:"NodeID,omitempty"`
	Status       TaskStatus `json:"Status,omitempty"`
	DesiredState string     `json:"DesiredState,omitempty"`
}

// TaskStatus is the runtime state of a task (monotonically advancing).
type TaskStatus struct {
	State           string          `json:"State"` // new,preparing,running,complete,failed,...
	Message         string          `json:"Message,omitempty"`
	Err             string          `json:"Err,omitempty"`
	Timestamp       string          `json:"Timestamp,omitempty"`
	ContainerStatus ContainerStatus `json:"ContainerStatus,omitempty"`
}

// ContainerStatus links the task to its container.
type ContainerStatus struct {
	ContainerID string `json:"ContainerID,omitempty"`
	PID         int    `json:"PID,omitempty"`
	ExitCode    int    `json:"ExitCode,omitempty"`
}

// Node is a swarm member.
type Node struct {
	ID            string          `json:"ID"`
	Version       Version         `json:"Version"`
	CreatedAt     string          `json:"CreatedAt,omitempty"`
	Spec          NodeSpec        `json:"Spec"`
	Description   NodeDescription `json:"Description"`
	Status        NodeStatus      `json:"Status"`
	ManagerStatus *ManagerStatus  `json:"ManagerStatus,omitempty"`
}

// NodeSpec is the desired node configuration.
type NodeSpec struct {
	Name         string            `json:"Name,omitempty"`
	Labels       map[string]string `json:"Labels,omitempty"`
	Role         string            `json:"Role"`         // manager | worker
	Availability string            `json:"Availability"` // active | pause | drain
}

// NodeDescription holds static node facts.
type NodeDescription struct {
	Hostname  string        `json:"Hostname"`
	Resources NodeResources `json:"Resources,omitempty"`
}

// NodeStatus is the node's runtime state.
type NodeStatus struct {
	State   string `json:"State"` // ready | down | disconnected | ...
	Message string `json:"Message,omitempty"`
	Addr    string `json:"Addr,omitempty"`
}

// NodeResources is the node's total resources.
type NodeResources struct {
	NanoCPUs    int64 `json:"NanoCPUs,omitempty"`
	MemoryBytes int64 `json:"MemoryBytes,omitempty"`
}

// ManagerStatus is non-nil only for manager nodes.
type ManagerStatus struct {
	Leader       bool   `json:"Leader"`
	Reachability string `json:"Reachability"` // reachable | unreachable
	Addr         string `json:"Addr,omitempty"`
}

// Container is a container summary from /containers/json.
type Container struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names,omitempty"`
	Image  string            `json:"Image,omitempty"`
	Labels map[string]string `json:"Labels,omitempty"`
	State  string            `json:"State,omitempty"`
	Status string            `json:"Status,omitempty"`
	Ports  []ContainerPort   `json:"Ports,omitempty"`
}

// ContainerPort is one host-published port mapping from /containers/json.
type ContainerPort struct {
	IP          string `json:"IP,omitempty"`
	PrivatePort uint16 `json:"PrivatePort"`
	PublicPort  uint16 `json:"PublicPort,omitempty"`
	Type        string `json:"Type"` // tcp | udp
}

// ContainerInspect is the subset of /containers/{id}/json Worker uses, mainly
// to read the container's Health.Status for readiness judgment.
type ContainerInspect struct {
	ID    string         `json:"Id"`
	Name  string         `json:"Name,omitempty"`
	State ContainerState `json:"State"`
}

// ContainerState holds runtime state.
type ContainerState struct {
	Status string  `json:"Status"` // running, exited, restarting, ...
	Health *Health `json:"Health,omitempty"`
}

// Health is the container healthcheck status.
type Health struct {
	Status string `json:"Status"` // starting, healthy, unhealthy, none
}

// Secret is a swarm secret (used for registry auth config.json references).
type Secret struct {
	ID   string     `json:"ID"`
	Spec SecretSpec `json:"Spec"`
}

// SecretSpec holds the secret's name and (base64-encoded) data.
type SecretSpec struct {
	Name string `json:"Name,omitempty"`
	Data string `json:"Data,omitempty"`
}

// Stats is a one-shot container stats snapshot.
type Stats struct {
	Name        string      `json:"name,omitempty"`
	CPUStats    CPUStats    `json:"cpu_stats"`
	MemoryStats MemoryStats `json:"memory_stats"`
}

// CPUStats holds CPU usage counters.
type CPUStats struct {
	CPUUsage       CPUUsage `json:"cpu_usage"`
	SystemCPUUsage uint64   `json:"system_cpu_usage,omitempty"`
	OnlineCPUs     uint32   `json:"online_cpus,omitempty"`
}

// CPUUsage holds the total CPU usage counter.
type CPUUsage struct {
	TotalUsage uint64 `json:"total_usage"`
}

// MemoryStats holds memory usage and limit.
type MemoryStats struct {
	Usage uint64            `json:"usage"`
	Limit uint64            `json:"limit"`
	Stats map[string]uint64 `json:"stats,omitempty"` // incl. "inactive_file"
}

// ---- Internal wire types ----

type serviceCreateResponse struct {
	ID       string   `json:"ID"`
	Warnings []string `json:"Warnings,omitempty"`
}

type versionInfo struct {
	Version    string `json:"Version"`
	APIVersion string `json:"ApiVersion,omitempty"`
}

// Info is the subset of /info Worker uses to identify the local node and its
// swarm role. Swarm.ControlAvailable is true when this daemon can run swarm
// control-plane operations (i.e. it is a manager).
type Info struct {
	Swarm SwarmInfo `json:"Swarm"`
}

// SwarmInfo carries the swarm membership of this daemon.
type SwarmInfo struct {
	NodeID           string `json:"NodeID"`
	NodeAddr         string `json:"NodeAddr"`
	LocalNodeState   string `json:"LocalNodeState"` // inactive | pending | active
	ControlAvailable bool   `json:"ControlAvailable"`
	RemoteManagers   []any  `json:"RemoteManagers"`
}

type apiError struct {
	Message string `json:"message"`
}
