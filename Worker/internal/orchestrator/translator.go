// Package orchestrator maps a Worker Config into Docker Engine API request
// structs (docker.ServiceSpec) and (in later phases) drives service lifecycle.
// P0 ships the translator only.
package orchestrator

import (
	"fmt"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
)

// Translate converts a validated Config into a docker.ServiceSpec request body.
//
// Note on image pull policy: the Engine API has no per-service PullMode field
// exposed in a stable way; the lifecycle layer performs an explicit ImagePull
// when policy == "always" (see design doc §4.4). This corrects the §4.3 table.
func Translate(cfg *config.Config) (docker.ServiceSpec, error) {
	s := cfg.Service
	spec := docker.ServiceSpec{}
	spec.Name = s.Name
	if s.Labels != nil {
		spec.Labels = s.Labels
	}

	// ---- ContainerSpec ----
	cs := docker.ContainerSpec{
		Image:   s.Image,
		Env:     s.Env,
		Command: s.Command,
		Args:    s.Args,
		Dir:     s.Workdir,
		User:    s.User,
	}
	for _, m := range s.Mounts {
		cs.Mounts = append(cs.Mounts, docker.Mount{
			Type:     m.Type,
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}
	if s.Healthcheck != nil {
		hc := &docker.HealthConfig{
			Test:    s.Healthcheck.Test,
			Retries: s.Healthcheck.Retries,
		}
		if d, err := time.ParseDuration(s.Healthcheck.Interval); err == nil {
			hc.Interval = d.Nanoseconds()
		}
		if d, err := time.ParseDuration(s.Healthcheck.Timeout); err == nil {
			hc.Timeout = d.Nanoseconds()
		}
		if d, err := time.ParseDuration(s.Healthcheck.StartPeriod); err == nil {
			hc.StartPeriod = d.Nanoseconds()
		}
		cs.Healthcheck = hc
	}
	for _, name := range s.Secrets {
		cs.Secrets = append(cs.Secrets, &docker.SecretReference{
			SecretName: name,
			File:       &docker.SecretReferenceFileTarget{Name: name, Mode: 0o444},
		})
	}
	for _, name := range s.Configs {
		cs.Configs = append(cs.Configs, &docker.ConfigReference{
			ConfigName: name,
			File:       &docker.ConfigReferenceFileTarget{Name: name, Mode: 0o444},
		})
	}
	// ---- SecurityOpt / PidMode ----
	if len(s.SecurityOpt) > 0 {
		cs.Privileges = &docker.Privileges{SecurityOpt: s.SecurityOpt}
	}
	cs.PidMode = s.PidMode

	spec.TaskTemplate.ContainerSpec = cs

	// ---- Resources ----
	if s.Resources != nil {
		rr := &docker.ResourceRequirements{}
		if s.Resources.Limits != nil {
			r, err := toDockerResources(s.Resources.Limits)
			if err != nil {
				return docker.ServiceSpec{}, fmt.Errorf("resources.limits: %w", err)
			}
			rr.Limits = r
		}
		if s.Resources.Reservations != nil {
			r, err := toDockerResources(s.Resources.Reservations)
			if err != nil {
				return docker.ServiceSpec{}, fmt.Errorf("resources.reservations: %w", err)
			}
			rr.Reservations = r
		}
		spec.TaskTemplate.Resources = rr
	}

	// ---- Restart policy ----
	if s.Restart != nil {
		rp := &docker.RestartPolicy{Condition: s.Restart.Condition}
		if s.Restart.Delay != "" {
			if d, err := time.ParseDuration(s.Restart.Delay); err == nil {
				rp.Delay = d.Nanoseconds()
			}
		}
		if s.Restart.MaxAttempts > 0 {
			rp.MaxAttempts = int64(s.Restart.MaxAttempts)
		}
		if s.Restart.Window != "" {
			if d, err := time.ParseDuration(s.Restart.Window); err == nil {
				rp.Window = d.Nanoseconds()
			}
		}
		spec.TaskTemplate.RestartPolicy = rp
	}

	// ---- Placement ----
	if s.Placement != nil && (len(s.Placement.Constraints) > 0 || len(s.Placement.Preferences) > 0) {
		pl := &docker.Placement{Constraints: s.Placement.Constraints}
		for _, p := range s.Placement.Preferences {
			if p.Spread != "" {
				pl.Preferences = append(pl.Preferences, docker.PlacementPreference{
					Spread: &docker.SpreadOverTag{SpreadDescriptor: p.Spread},
				})
			}
		}
		spec.TaskTemplate.Placement = pl
	}

	// ---- Log driver ----
	if s.LogDriver != nil {
		spec.TaskTemplate.LogDriver = &docker.Driver{
			Name:    s.LogDriver.Name,
			Options: s.LogDriver.Options,
		}
	}

	// ---- Mode ----
	switch s.Mode {
	case "global":
		spec.Mode = docker.ServiceMode{Global: &docker.GlobalService{}}
	default: // replicated (ApplyDefaults guarantees Mode != "")
		reps := uint64(1)
		if s.Replicas != nil {
			reps = *s.Replicas
		}
		spec.Mode = docker.ServiceMode{Replicated: &docker.ReplicatedService{Replicas: &reps}}
	}

	// ---- Update / Rollback ----
	if s.Update != nil {
		spec.UpdateConfig = toDockerUpdateConfig(s.Update)
	}
	if s.Rollback != nil {
		spec.RollbackConfig = toDockerUpdateConfig(s.Rollback)
	}

	// ---- Networks ----
	for _, n := range s.Networks {
		spec.Networks = append(spec.Networks, docker.NetworkAttachmentConfig{Target: n})
	}

	// ---- Endpoint / Ports ----
	if len(s.Ports) > 0 {
		ep := &docker.EndpointSpec{}
		for _, p := range s.Ports {
			ep.Ports = append(ep.Ports, docker.PortConfig{
				PublishedPort: p.Published,
				TargetPort:    p.Target,
				Protocol:      p.Protocol,
				PublishMode:   p.Mode,
			})
		}
		spec.EndpointSpec = ep
	}

	return spec, nil
}

// toDockerResources converts CPU/memory quantity strings into docker.Resources
// (NanoCPUs / MemoryBytes). Empty strings are left as zero.
func toDockerResources(q *config.ResourceQuantities) (*docker.Resources, error) {
	r := &docker.Resources{}
	if q.CPU != "" {
		n, err := config.ParseCPU(q.CPU)
		if err != nil {
			return nil, err
		}
		r.NanoCPUs = n
	}
	if q.Memory != "" {
		n, err := config.ParseMemory(q.Memory)
		if err != nil {
			return nil, err
		}
		r.MemoryBytes = n
	}
	return r, nil
}

// toDockerUpdateConfig maps config.UpdateConfig to docker.UpdateConfig. Invalid
// durations are silently dropped (config.Validate should have caught them).
func toDockerUpdateConfig(u *config.UpdateConfig) *docker.UpdateConfig {
	sc := &docker.UpdateConfig{
		Parallelism:     u.Parallelism,
		FailureAction:   u.FailureAction,
		MaxFailureRatio: u.MaxFailureRatio,
	}
	if d, err := time.ParseDuration(u.Delay); err == nil {
		sc.Delay = d.Nanoseconds()
	}
	if d, err := time.ParseDuration(u.Monitor); err == nil {
		sc.Monitor = d.Nanoseconds()
	}
	return sc
}
