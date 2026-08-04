package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Validate checks the config for semantic errors. ApplyDefaults should be
// called first. It returns a multi-line error describing all problems found.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("config is nil")
	}
	var problems []string

	// ---- Service ----
	s := c.Service
	if s.Name == "" {
		problems = append(problems, "service.name is required")
	} else if !svcNameRe.MatchString(s.Name) {
		problems = append(problems, fmt.Sprintf("service.name %q must match [a-z0-9][a-z0-9_.-]{0,62}", s.Name))
	}
	if s.Image == "" {
		problems = append(problems, "service.image is required")
	}
	switch s.Mode {
	case "replicated", "global":
	default:
		problems = append(problems, fmt.Sprintf("service.mode %q must be replicated|global", s.Mode))
	}
	if s.Mode == "replicated" && s.Replicas != nil && *s.Replicas == 0 {
		// 0 is the "stop" sentinel; allowed only via explicit scale, but reject in deploy config.
		problems = append(problems, "service.replicas must be > 0 (use scale_service to stop)")
	}
	switch s.ImagePullPolicy {
	case "", "always", "missing", "never":
	default:
		problems = append(problems, fmt.Sprintf("service.imagePullPolicy %q must be always|missing|never", s.ImagePullPolicy))
	}
	if s.RegistryAuth != nil && s.RegistryAuth.SecretRef == "" && s.RegistryAuth.Inline == "" {
		problems = append(problems, "service.registryAuth requires secretRef or inline")
	}
	for i, p := range s.Ports {
		if p.Target == 0 {
			problems = append(problems, fmt.Sprintf("service.ports[%d].target is required", i))
		}
		switch p.Protocol {
		case "", "tcp", "udp", "sctp":
		default:
			problems = append(problems, fmt.Sprintf("service.ports[%d].protocol %q invalid", i, p.Protocol))
		}
		switch p.Mode {
		case "", "ingress", "host":
		default:
			problems = append(problems, fmt.Sprintf("service.ports[%d].mode %q invalid", i, p.Mode))
		}
	}
	for i, m := range s.Mounts {
		if m.Type == "" {
			problems = append(problems, fmt.Sprintf("service.mounts[%d].type is required", i))
		}
		if m.Target == "" {
			problems = append(problems, fmt.Sprintf("service.mounts[%d].target is required", i))
		}
	}
	if s.Healthcheck != nil && len(s.Healthcheck.Test) == 0 {
		problems = append(problems, "service.healthcheck.test is required when healthcheck is set")
	}
	if s.Healthcheck != nil {
		if _, err := parseDurationField(s.Healthcheck.Interval); err != nil {
			problems = append(problems, "service.healthcheck.interval: "+err.Error())
		}
		if _, err := parseDurationField(s.Healthcheck.Timeout); err != nil {
			problems = append(problems, "service.healthcheck.timeout: "+err.Error())
		}
		if _, err := parseDurationField(s.Healthcheck.StartPeriod); err != nil {
			problems = append(problems, "service.healthcheck.startPeriod: "+err.Error())
		}
	}
	if s.Update != nil {
		problems = append(problems, validateUpdate("service.update", *s.Update)...)
	}
	if s.Rollback != nil {
		problems = append(problems, validateUpdate("service.rollback", *s.Rollback)...)
	}
	if s.Restart != nil {
		switch s.Restart.Condition {
		case "", "any", "on-failure", "none":
		default:
			problems = append(problems, fmt.Sprintf("service.restart.condition %q invalid", s.Restart.Condition))
		}
		for _, f := range []struct{ name, val string }{
			{"delay", s.Restart.Delay}, {"window", s.Restart.Window},
		} {
			if _, err := parseDurationField(f.val); err != nil {
				problems = append(problems, fmt.Sprintf("service.restart.%s: %s", f.name, err.Error()))
			}
		}
	}
	if s.Resources != nil {
		if s.Resources.Limits != nil {
			problems = append(problems, validateQty("service.resources.limits", *s.Resources.Limits)...)
		}
		if s.Resources.Reservations != nil {
			problems = append(problems, validateQty("service.resources.reservations", *s.Resources.Reservations)...)
		}
	}

	// ---- Monitoring ----
	m := c.Monitoring
	for i, pc := range m.PortChecks {
		if pc.Port == "" {
			problems = append(problems, fmt.Sprintf("monitoring.portChecks[%d].port is required", i))
		}
		if _, err := parseDurationField(pc.Interval); err != nil {
			problems = append(problems, fmt.Sprintf("monitoring.portChecks[%d].interval: %s", i, err.Error()))
		}
		if _, err := parseDurationField(pc.Timeout); err != nil {
			problems = append(problems, fmt.Sprintf("monitoring.portChecks[%d].timeout: %s", i, err.Error()))
		}
	}
	for i, hc := range m.HTTPChecks {
		if hc.URL == "" {
			problems = append(problems, fmt.Sprintf("monitoring.httpChecks[%d].url is required", i))
		} else if _, err := url.Parse(hc.URL); err != nil {
			problems = append(problems, fmt.Sprintf("monitoring.httpChecks[%d].url: %s", i, err.Error()))
		}
		if hc.ExpectedBody != "" {
			if _, err := regexp.Compile(hc.ExpectedBody); err != nil {
				problems = append(problems, fmt.Sprintf("monitoring.httpChecks[%d].expectedBody: %s", i, err.Error()))
			}
		}
		if _, err := parseDurationField(hc.Interval); err != nil {
			problems = append(problems, fmt.Sprintf("monitoring.httpChecks[%d].interval: %s", i, err.Error()))
		}
		if _, err := parseDurationField(hc.Timeout); err != nil {
			problems = append(problems, fmt.Sprintf("monitoring.httpChecks[%d].timeout: %s", i, err.Error()))
		}
	}
	for i, lc := range m.LogChecks {
		if lc.Pattern == "" {
			problems = append(problems, fmt.Sprintf("monitoring.logChecks[%d].pattern is required", i))
		} else if _, err := regexp.Compile(lc.Pattern); err != nil {
			problems = append(problems, fmt.Sprintf("monitoring.logChecks[%d].pattern: %s", i, err.Error()))
		}
		for j, ig := range lc.Ignore {
			if _, err := regexp.Compile(ig); err != nil {
				problems = append(problems, fmt.Sprintf("monitoring.logChecks[%d].ignore[%d]: %s", i, j, err.Error()))
			}
		}
		switch lc.Action {
		case "", "alert", "restart":
		default:
			problems = append(problems, fmt.Sprintf("monitoring.logChecks[%d].action %q invalid", i, lc.Action))
		}
	}
	for i, rt := range m.ResourceThresholds {
		switch rt.Metric {
		case "cpu", "memory":
		default:
			problems = append(problems, fmt.Sprintf("monitoring.resourceThresholds[%d].metric %q must be cpu|memory", i, rt.Metric))
		}
		if rt.Threshold < 0 || rt.Threshold > 100 {
			problems = append(problems, fmt.Sprintf("monitoring.resourceThresholds[%d].threshold %d must be 0-100", i, rt.Threshold))
		}
		switch rt.Action {
		case "", "alert", "restart":
		default:
			problems = append(problems, fmt.Sprintf("monitoring.resourceThresholds[%d].action %q invalid", i, rt.Action))
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return errors.New("config validation failed:\n  - " + strings.Join(problems, "\n  - "))
}

func validateUpdate(prefix string, u UpdateConfig) []string {
	var p []string
	switch u.FailureAction {
	case "", "pause", "continue", "rollback":
	default:
		p = append(p, fmt.Sprintf("%s.failureAction %q invalid", prefix, u.FailureAction))
	}
	for _, f := range []struct{ name, val string }{
		{"delay", u.Delay}, {"monitor", u.Monitor},
	} {
		if _, err := parseDurationField(f.val); err != nil {
			p = append(p, fmt.Sprintf("%s.%s: %s", prefix, f.name, err.Error()))
		}
	}
	return p
}

func validateQty(prefix string, q ResourceQuantities) []string {
	var p []string
	if q.CPU != "" {
		if _, err := ParseCPU(q.CPU); err != nil {
			p = append(p, fmt.Sprintf("%s.cpu: %s", prefix, err.Error()))
		}
	}
	if q.Memory != "" {
		if _, err := ParseMemory(q.Memory); err != nil {
			p = append(p, fmt.Sprintf("%s.memory: %s", prefix, err.Error()))
		}
	}
	return p
}

// parseDurationField parses a duration string, treating "" as zero (no error).
func parseDurationField(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

var svcNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
