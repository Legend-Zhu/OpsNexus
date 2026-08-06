package grpcapi

import (
	"context"
	"io"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/audit"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/grpcapi/pb"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/monitor"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SubscribeEvents streams monitor events to the server. The server opens the
// bidirectional stream and sends SubscribeRequest messages:
//   - the first message carries after_seq (resume cursor; 0 = from start);
//   - subsequent messages carry ack_seq to acknowledge delivered events so the
//     Worker can garbage-collect persisted entries.
//
// The Worker blocks on EventStore.WaitNew (woken on Add or ctx cancel) and
// pushes events oldest-first. The stream stays open until the server closes it
// (disconnect/reconnect) or the context is cancelled.
func (s *Server) SubscribeEvents(stream pb.ManagementService_SubscribeEventsServer) error {
	ctx := stream.Context()

	// Read the initial cursor. The server must send one SubscribeRequest first.
	init, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.Unavailable, "await initial cursor: %v", err)
	}
	cursor := init.GetAfterSeq()

	// Background ack receiver: best-effort, never block the send loop. Ack
	// failures are non-fatal (events just stay un-GC'd until the next ack).
	go s.recvEventAcks(ctx, stream)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		evs, err := s.events.WaitNew(ctx, cursor)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return status.Error(codes.Internal, err.Error())
		}
		for _, e := range evs {
			if err := stream.Send(eventToPB(e)); err != nil {
				return err // stream broken; server will reconnect + resume
			}
			cursor = e.Seq
		}
	}
}

// recvEventAcks drains the ack channel from the server and applies them to the
// event store. It exits when the stream context is done.
func (s *Server) recvEventAcks(ctx context.Context, stream pb.ManagementService_SubscribeEventsServer) {
	for {
		if ctx.Err() != nil {
			return
		}
		req, err := stream.Recv()
		if err != nil {
			if err != io.EOF {
				s.log.Debug("event ack stream closed", "err", err)
			}
			return
		}
		if req.GetAckSeq() > 0 {
			if err := s.events.Ack(req.GetAckSeq()); err != nil {
				s.log.Warn("event ack failed", "seq", req.GetAckSeq(), "err", err)
			}
		}
	}
}

// SubscribeAudit mirrors SubscribeEvents for audit entries.
func (s *Server) SubscribeAudit(stream pb.ManagementService_SubscribeAuditServer) error {
	ctx := stream.Context()
	init, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.Unavailable, "await initial cursor: %v", err)
	}
	cursor := init.GetAfterSeq()

	go s.recvAuditAcks(ctx, stream)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		entries, err := s.audit.WaitNew(ctx, cursor)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return status.Error(codes.Internal, err.Error())
		}
		for _, e := range entries {
			if err := stream.Send(auditToPB(e)); err != nil {
				return err
			}
			cursor = e.Seq
		}
	}
}

func (s *Server) recvAuditAcks(ctx context.Context, stream pb.ManagementService_SubscribeAuditServer) {
	for {
		if ctx.Err() != nil {
			return
		}
		req, err := stream.Recv()
		if err != nil {
			if err != io.EOF {
				s.log.Debug("audit ack stream closed", "err", err)
			}
			return
		}
		if req.GetAckSeq() > 0 {
			if err := s.audit.Ack(req.GetAckSeq()); err != nil {
				s.log.Warn("audit ack failed", "seq", req.GetAckSeq(), "err", err)
			}
		}
	}
}

// StreamLogs streams a service's log lines (server-streaming). With follow=true
// the stream stays open with reconnect-on-EOF, mirroring the SSE behavior.
func (s *Server) StreamLogs(req *pb.StreamLogsRequest, stream pb.ManagementService_StreamLogsServer) error {
	ctx := stream.Context()
	service := req.GetService()
	if service == "" {
		return status.Error(codes.InvalidArgument, "service is required")
	}
	tail := ""
	if req.GetTail() > 0 {
		tail = itoa(req.GetTail())
	}

	emit := func(line string) {
		_ = stream.Send(&pb.LogLine{
			Ts:     time.Now().UTC().Format(time.RFC3339),
			Stream: "stdout",
			Line:   line,
		})
	}

	if !req.GetFollow() {
		return s.streamOnce(ctx, service, tail, req.GetSince(), emit)
	}
	// follow: reconnect on EOF with bounded backoff (matches nodeagent).
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.streamOnce(ctx, service, tail, req.GetSince(), emit); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// streamOnce opens the Docker service log stream once, decodes lines, and
// emits them. Returns nil on natural EOF.
func (s *Server) streamOnce(ctx context.Context, service, tail, since string, emit func(string)) error {
	rc, err := s.local.ServiceLogs(ctx, service, docker.LogsOptions{
		Stdout: true, Stderr: true, Tail: tail, Since: since,
	})
	if err != nil {
		return status.Error(codes.Unavailable, err.Error())
	}
	defer rc.Close()
	return docker.DecodeLogLines(rc, emit)
}

// ---- mappers ----

func eventToPB(e monitor.Event) *pb.MonitorEvent {
	return &pb.MonitorEvent{
		Seq:     e.Seq,
		Id:      e.ID,
		Ts:      timestamppb.New(e.TS),
		Service: e.Service,
		Type:    string(e.Type),
		Level:   string(e.Level),
		Msg:     e.Msg,
		Detail:  e.Detail,
	}
}

func auditToPB(e audit.Entry) *pb.AuditEntry {
	return &pb.AuditEntry{
		Seq:     e.Seq,
		Id:      e.ID,
		Ts:      timestamppb.New(e.TS),
		Actor:   e.Actor,
		Action:  string(e.Action),
		Service: e.Service,
		Command: e.Command,
		Target:  e.Target,
		Ok:      e.OK,
		Detail:  e.Detail,
	}
}

func itoa(n int32) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
