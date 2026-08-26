package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/config"
)

// MonitorConfigStore persists the monitoring block last delivered to each
// service (service name → Monitoring), so the Worker can rebuild monitor jobs
// after a restart — jobs live only in monitor.Manager memory and the Docker
// service spec does not carry the monitoring block. Writes are atomic
// (temp file + rename); a disabled block is persisted too, so a restarted
// Worker never resurrects monitoring that was explicitly turned off.
type MonitorConfigStore struct {
	path string
	mu   sync.Mutex
}

// NewMonitorConfigStore creates a store backed by the JSON file at path.
func NewMonitorConfigStore(path string) *MonitorConfigStore {
	return &MonitorConfigStore{path: path}
}

type monitorConfigFile struct {
	Services map[string]config.Monitoring `json:"services"`
}

// load reads the persisted map; a missing file yields an empty map.
// Callers must hold s.mu.
func (s *MonitorConfigStore) load() (monitorConfigFile, error) {
	var f monitorConfigFile
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return monitorConfigFile{Services: map[string]config.Monitoring{}}, nil
		}
		return f, fmt.Errorf("read monitor configs: %w", err)
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("decode monitor configs %s: %w", s.path, err)
	}
	if f.Services == nil {
		f.Services = map[string]config.Monitoring{}
	}
	return f, nil
}

// save atomically writes the map. Callers must hold s.mu.
func (s *MonitorConfigStore) save(f monitorConfigFile) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encode monitor configs: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create monitor config dir: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write monitor configs: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace monitor configs: %w", err)
	}
	return nil
}

// Put persists a service's monitoring block (overwrites any previous value,
// including replacing an enabled block with a disabled one).
func (s *MonitorConfigStore) Put(service string, m config.Monitoring) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	f.Services[service] = m
	return s.save(f)
}

// Delete removes a service's persisted monitoring block (service removed).
// Deleting a missing entry is a no-op.
func (s *MonitorConfigStore) Delete(service string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := f.Services[service]; !ok {
		return nil
	}
	delete(f.Services, service)
	return s.save(f)
}

// All returns the full persisted map (empty map when the file is absent).
func (s *MonitorConfigStore) All() (map[string]config.Monitoring, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	return f.Services, nil
}
