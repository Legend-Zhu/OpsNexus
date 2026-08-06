// Event/audit subscription over the gRPC management API. Replaces the former
// Worker→server webhook push: the server now opens a bidirectional
// SubscribeEvents stream to each cluster's Worker (honoring the one-way network
// policy — the server initiates the connection), drains events, persists them
// via ingest.HandleEvent, acks each delivered seq, and saves the cursor so a
// reconnect or server restart resumes without gaps or duplicates.
package ingest

import (
	"context"
	"log/slog"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
	pb "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy/pb"
)

// ClusterClientFactory builds a per-cluster Worker client + cluster name, used
// to decouple the subscriber from the cluster registry (testable). The caller
// closes the returned close func when the subscription stops.
type ClusterClientFactory func(cluster string) (sub EventSubscriber, closeFn func(), err error)

// EventSubscriber is the minimal gRPC streaming surface the subscriber needs.
// workerproxy.EventSubscription satisfies it.
type EventSubscriber interface {
	Recv() (*pb.MonitorEvent, error)
	Ack(seq int64) error
}

// Subscriber drains monitor events for one cluster over a gRPC SubscribeEvents
// stream, persists them, and acks. Reconnects with bounded backoff; the cursor
// is persisted after every ack so a restart resumes cleanly.
type Subscriber struct {
	cluster string
	factory ClusterClientFactory
	svc     *Service
	st      *store.Store
	log     *slog.Logger
}

// NewSubscriber creates a per-cluster event subscriber.
func NewSubscriber(cluster string, factory ClusterClientFactory, svc *Service, st *store.Store, log *slog.Logger) *Subscriber {
	if log == nil {
		log = slog.Default()
	}
	return &Subscriber{cluster: cluster, factory: factory, svc: svc, st: st, log: log}
}

// Run subscribes and drains events until ctx is cancelled. It reconnects on
// stream errors, resuming from the persisted cursor each time.
func (s *Subscriber) Run(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := s.subscribeOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.log.Warn("event subscription ended, reconnecting", "cluster", s.cluster, "err", err, "backoff", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// subscribeOnce opens one stream, drains it until error/EOF, and returns the
// error (nil on clean ctx cancel).
func (s *Subscriber) subscribeOnce(ctx context.Context) error {
	sub, closeFn, err := s.factory(s.cluster)
	if err != nil {
		return err
	}
	defer closeFn()
	// The cursor is loaded by the factory when opening the stream (it resumes
	// from the last persisted position). We only advance + persist it here as
	// events arrive.

	for {
		if ctx.Err() != nil {
			return nil
		}
		ev, err := sub.Recv()
		if err != nil {
			return err
		}
		if err := s.svc.HandleEvent(s.cluster, pbEventToIngest(ev)); err != nil {
			s.log.Warn("handle event failed", "cluster", s.cluster, "seq", ev.GetSeq(), "err", err)
			// Do NOT ack on persist failure — the Worker will redeliver on reconnect.
			continue
		}
		// Ack + persist cursor. Persist first so a crash after ack doesn't
		// lose the cursor (worst case: redeliver a few events, dedup handles it).
		if err := s.st.PutCursor(s.cluster, ev.GetSeq()); err != nil {
			s.log.Warn("persist cursor failed", "cluster", s.cluster, "err", err)
		}
		if err := sub.Ack(ev.GetSeq()); err != nil {
			s.log.Warn("ack failed", "cluster", s.cluster, "seq", ev.GetSeq(), "err", err)
			return err // stream likely broken; reconnect will resume from cursor
		}
	}
}

// pbEventToIngest converts a protobuf monitor event into the store's IngestEvent
// shape that ingest.Service.HandleEvent expects.
func pbEventToIngest(e *pb.MonitorEvent) *store.IngestEvent {
	var ts time.Time
	if e.GetTs() != nil {
		ts = e.GetTs().AsTime()
	} else {
		ts = time.Now().UTC()
	}
	return &store.IngestEvent{
		ID:      e.GetId(),
		TS:      ts,
		Service: e.GetService(),
		Type:    store.EventType(e.GetType()),
		Level:   store.Level(e.GetLevel()),
		Msg:     e.GetMsg(),
		Detail:  e.GetDetail(),
	}
}
