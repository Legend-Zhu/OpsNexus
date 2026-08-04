package config

import (
	"strings"
	"testing"
)

func TestParseMemory(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"512Mi", 536870912, false},
		{"1G", 1073741824, false},
		{"256m", 268435456, false},
		{"1024", 1024, false},
		{"2Gi", 2147483648, false},
		{"1.5Gi", 1610612736, false},
		{"1t", 1099511627776, false},
		{"", 0, true},
		{"abc", 0, true},
		{"5Xi", 0, true}, // unknown suffix
	}
	for _, tc := range cases {
		got, err := parseMemory(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("parseMemory(%q) want error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMemory(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseMemory(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseCPU(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"1.0", 1000000000, false},
		{"0.5", 500000000, false},
		{"500m", 500000000, false},
		{"2000m", 2000000000, false},
		{"2", 2000000000, false},
		{"", 0, true},
		{"abc", 0, true},
		{"-1", 0, true},
	}
	for _, tc := range cases {
		got, err := parseCPU(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("parseCPU(%q) want error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseCPU(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseCPU(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestApplyDefaults(t *testing.T) {
	yml := `
service:
  name: web
  image: nginx:alpine
monitoring:
  enabled: true
  portChecks:
    - { port: "8080" }
  httpChecks:
    - { url: "http://localhost/health" }
  logChecks:
    - { pattern: "ERROR" }
  resourceThresholds:
    - { metric: cpu, threshold: 80 }
`
	c, err := LoadBytes("c.yaml", []byte(yml))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if c.Service.Mode != "replicated" {
		t.Errorf("default mode = %q, want replicated", c.Service.Mode)
	}
	if c.Service.Replicas == nil || *c.Service.Replicas != 1 {
		t.Errorf("default replicas = %v, want 1", c.Service.Replicas)
	}
	if c.Service.ImagePullPolicy != "missing" {
		t.Errorf("default imagePullPolicy = %q, want missing", c.Service.ImagePullPolicy)
	}
	if c.Monitoring.PortChecks[0].Protocol != "tcp" {
		t.Errorf("default port protocol = %q", c.Monitoring.PortChecks[0].Protocol)
	}
	if c.Monitoring.PortChecks[0].Interval != "10s" {
		t.Errorf("default interval = %q", c.Monitoring.PortChecks[0].Interval)
	}
	if c.Monitoring.HTTPChecks[0].Method != "GET" {
		t.Errorf("default http method = %q", c.Monitoring.HTTPChecks[0].Method)
	}
	if c.Monitoring.LogChecks[0].Action != "alert" {
		t.Errorf("default log action = %q", c.Monitoring.LogChecks[0].Action)
	}
	if c.Monitoring.ResourceThresholds[0].Action != "alert" {
		t.Errorf("default threshold action = %q", c.Monitoring.ResourceThresholds[0].Action)
	}
}

func TestValidate_OK(t *testing.T) {
	yml := `
service:
  name: my-app
  image: registry.example.com/app:v1
  mode: replicated
  replicas: 3
  ports:
    - { published: 8080, target: 80, mode: ingress }
  resources:
    limits: { cpu: "1.0", memory: "512Mi" }
    reservations: { cpu: "0.5", memory: "256Mi" }
  healthcheck:
    test: ["CMD-SHELL", "curl -f http://localhost:8080/health || exit 1"]
    interval: 10s
    timeout: 5s
    retries: 3
    startPeriod: 30s
  update:
    parallelism: 1
    delay: 10s
    failureAction: rollback
    monitor: 30s
  restart:
    condition: any
    delay: 5s
    maxAttempts: 3
    window: 120s
monitoring:
  enabled: true
  portChecks:
    - { port: "8080", interval: 10s, timeout: 3s }
  httpChecks:
    - { url: "http://localhost:8080/health", method: GET, expectedStatus: [200] }
  logChecks:
    - { pattern: "ERROR|panic", action: alert }
  resourceThresholds:
    - { metric: cpu, threshold: 80 }
    - { metric: memory, threshold: 85 }
`
	if _, err := LoadBytes("c.yaml", []byte(yml)); err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
}

func TestValidate_GlobalStripsReplicas(t *testing.T) {
	yml := `
service:
  name: agent
  image: my/agent:latest
  mode: global
  replicas: 5
`
	c, err := LoadBytes("c.yaml", []byte(yml))
	if err != nil {
		t.Fatalf("global config should be valid: %v", err)
	}
	if c.Service.Replicas != nil {
		t.Errorf("global mode must strip replicas, got %d", *c.Service.Replicas)
	}
}

func TestValidate_Errors(t *testing.T) {
	cases := []struct {
		name    string
		yml     string
		wantSub string
	}{
		{"missing name", `service: { image: x }`, "service.name is required"},
		{"bad name", `service: { name: "Bad Name", image: x }`, "service.name"},
		{"bad mode", `service: { name: a, image: x, mode: foo }`, "service.mode"},
		{"zero replicas", `service: { name: a, image: x, replicas: 0 }`, "replicas must be > 0"},
		{"bad pull policy", `service: { name: a, image: x, imagePullPolicy: force }`, "imagePullPolicy"},
		{"port no target", `service: { name: a, image: x, ports: [{published: 1}] }`, "target is required"},
		{"bad cpu", `service: { name: a, image: x, resources: { limits: { cpu: "abc" } } }`, "limits.cpu"},
		{"bad memory", `service: { name: a, image: x, resources: { limits: { memory: "12Xi" } } }`, "limits.memory"},
		{"bad duration", `service: { name: a, image: x, healthcheck: { test: ["CMD","x"], interval: "abc" } }`, "healthcheck.interval"},
		{"bad regex", "monitoring: { logChecks: [{ pattern: '[' }] }", "logChecks[0].pattern"},
		{"bad threshold metric", "monitoring: { resourceThresholds: [{ metric: disk, threshold: 50 }] }", "metric"},
		{"bad threshold range", "monitoring: { resourceThresholds: [{ metric: cpu, threshold: 150 }] }", "threshold"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Each case needs at least a service block to avoid "image required"
			// masking the real error; add minimal defaults where the error is in
			// monitoring only.
			full := tc.yml
			if strings.Contains(tc.name, "monitoring") || strings.Contains(tc.name, "regex") {
				full = "service: { name: a, image: x }\n" + tc.yml
			}
			_, err := LoadBytes("c.yaml", []byte(full))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}
