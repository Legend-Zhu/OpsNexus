package orchestrator

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"sync"
	"time"
)

// OperationType enumerates the lifecycle actions tracked by the store.
type OperationType string

const (
	OpCreate   OperationType = "create"
	OpUpdate   OperationType = "update"
	OpScale    OperationType = "scale"
	OpRestart  OperationType = "restart"
	OpRollback OperationType = "rollback"
	OpRemove   OperationType = "remove"
)

// OperationStatus is the high-level outcome of a lifecycle action.
type OperationStatus string

const (
	// OpStatusPending means the action is still converging.
	OpStatusPending OperationStatus = "pending"
	// OpStatusHealthy means all replicas are running and (if configured) healthy.
	OpStatusHealthy OperationStatus = "healthy"
	// OpStatusPartial means 0 < running < desired; the service is degraded.
	OpStatusPartial OperationStatus = "partial"
	// OpStatusFailed means the action could not converge in time or errored.
	OpStatusFailed OperationStatus = "failed"
	// OpStatusDone means a non-health action (remove/restart) completed.
	OpStatusDone OperationStatus = "done"
)

// Operation is a tracked lifecycle action. It is JSON-serializable for the
// HTTP API (GET /api/v1/operations/{id}).
type Operation struct {
	ID         string           `json:"id"`
	Type       OperationType    `json:"type"`
	Service    string           `json:"service"`
	Status     OperationStatus  `json:"status"`
	StartedAt  time.Time        `json:"startedAt"`
	FinishedAt *time.Time       `json:"finishedAt,omitempty"`
	ServiceID  string           `json:"serviceId,omitempty"`
	Error      string           `json:"error,omitempty"`
	Steps      []string         `json:"steps,omitempty"`
	Replicas   uint64           `json:"replicas,omitempty"`
	Mode       string           `json:"mode,omitempty"`

	// mu guards Status/Steps/FinishedAt against concurrent polling writes and
	// snapshot reads. It is a pointer so snapshots (value copies) share it
	// without copying a mutex (which go vet flags).
	mu *sync.Mutex
}

// AppendStep records a human-readable progress line.
func (o *Operation) AppendStep(step string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.Steps = append(o.Steps, time.Now().UTC().Format(time.RFC3339)+" "+step)
}

// SetStatus transitions the operation, finishing it when terminal.
func (o *Operation) SetStatus(s OperationStatus, errMsg string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.Status = s
	o.Error = errMsg
	if s == OpStatusHealthy || s == OpStatusFailed || s == OpStatusDone || s == OpStatusPartial {
		if o.FinishedAt == nil {
			now := time.Now().UTC()
			o.FinishedAt = &now
		}
	}
}

// snapshot returns a value copy safe for JSON marshalling. The copy shares
// o's mutex pointer (no lock is copied).
func (o *Operation) snapshot() Operation {
	o.mu.Lock()
	defer o.mu.Unlock()
	steps := make([]string, len(o.Steps))
	copy(steps, o.Steps)
	clone := *o
	clone.Steps = steps
	if o.FinishedAt != nil {
		t := *o.FinishedAt
		clone.FinishedAt = &t
	}
	return clone
}

// newOperation constructs a pending operation with a fresh mutex and id. The
// extra fields (ServiceID/Replicas/Mode) are set by the caller.
func newOperation(typ OperationType, service string) *Operation {
	return &Operation{
		ID:        newOpID(),
		Type:      typ,
		Service:   service,
		Status:    OpStatusPending,
		StartedAt: time.Now().UTC(),
		mu:        &sync.Mutex{},
	}
}

// newOpID returns a short random hex id (falls back to a timestamp on rand
// failure, which should not happen in practice).
func newOpID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// OperationStore is an in-memory, thread-safe registry of operations. It
// keeps insertion order for listing and caps history to the configured size.
type OperationStore struct {
	mu      sync.Mutex
	ops     map[string]*Operation
	order   []string
	maxKeep int
}

// NewOperationStore creates a store retaining the last maxKeep operations.
func NewOperationStore(maxKeep int) *OperationStore {
	if maxKeep <= 0 {
		maxKeep = 1000
	}
	return &OperationStore{ops: make(map[string]*Operation), maxKeep: maxKeep}
}

// Put stores a new operation and evicts the oldest if over capacity.
func (s *OperationStore) Put(op *Operation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops[op.ID] = op
	s.order = append(s.order, op.ID)
	for len(s.order) > s.maxKeep {
		old := s.order[0]
		delete(s.ops, old)
		s.order = s.order[1:]
	}
}

// Get returns a snapshot of an operation by ID.
func (s *OperationStore) Get(id string) (Operation, bool) {
	s.mu.Lock()
	op, ok := s.ops[id]
	s.mu.Unlock()
	if !ok {
		return Operation{}, false
	}
	return op.snapshot(), true
}

// List returns snapshots of all tracked operations, oldest first.
func (s *OperationStore) List() []Operation {
	s.mu.Lock()
	ids := make([]string, len(s.order))
	copy(ids, s.order)
	s.mu.Unlock()
	out := make([]Operation, 0, len(ids))
	for _, id := range ids {
		if op, ok := s.Get(id); ok {
			out = append(out, op)
		}
	}
	return out
}
