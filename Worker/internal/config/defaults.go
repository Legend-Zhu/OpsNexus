package config

// ApplyDefaults fills in zero values with sensible defaults. It does not
// validate; call Validate afterwards. It mutates the receiver in place.
func (c *Config) ApplyDefaults() {
	if c == nil {
		return
	}
	c.Service.applyDefaults()
	c.Monitoring.applyDefaults()
}

func (s *Service) applyDefaults() {
	if s.Mode == "" {
		s.Mode = "replicated"
	}
	if s.Mode == "replicated" && s.Replicas == nil {
		r := uint64(1)
		s.Replicas = &r
	}
	if s.ImagePullPolicy == "" {
		s.ImagePullPolicy = "missing"
	}
	for i := range s.Ports {
		if s.Ports[i].Protocol == "" {
			s.Ports[i].Protocol = "tcp"
		}
		if s.Ports[i].Mode == "" {
			s.Ports[i].Mode = "ingress"
		}
	}
	if s.Update != nil {
		if s.Update.Parallelism == 0 {
			s.Update.Parallelism = 1
		}
		if s.Update.FailureAction == "" {
			s.Update.FailureAction = "pause"
		}
	}
	if s.Rollback != nil {
		if s.Rollback.FailureAction == "" {
			s.Rollback.FailureAction = "pause"
		}
	}
	if s.Restart != nil {
		if s.Restart.Condition == "" {
			s.Restart.Condition = "any"
		}
	}
	if s.LogDriver != nil && s.LogDriver.Name == "" {
		s.LogDriver.Name = "json-file"
	}
}

func (m *Monitoring) applyDefaults() {
	for i := range m.PortChecks {
		if m.PortChecks[i].Protocol == "" {
			m.PortChecks[i].Protocol = "tcp"
		}
		if m.PortChecks[i].Interval == "" {
			m.PortChecks[i].Interval = "10s"
		}
		if m.PortChecks[i].Timeout == "" {
			m.PortChecks[i].Timeout = "3s"
		}
		if m.PortChecks[i].Retries == 0 {
			m.PortChecks[i].Retries = 2
		}
	}
	for i := range m.HTTPChecks {
		if m.HTTPChecks[i].Method == "" {
			m.HTTPChecks[i].Method = "GET"
		}
		if m.HTTPChecks[i].Interval == "" {
			m.HTTPChecks[i].Interval = "15s"
		}
		if m.HTTPChecks[i].Timeout == "" {
			m.HTTPChecks[i].Timeout = "5s"
		}
	}
	for i := range m.LogChecks {
		if m.LogChecks[i].Action == "" {
			m.LogChecks[i].Action = "alert"
		}
	}
	for i := range m.ResourceThresholds {
		if m.ResourceThresholds[i].Action == "" {
			m.ResourceThresholds[i].Action = "alert"
		}
	}
}
