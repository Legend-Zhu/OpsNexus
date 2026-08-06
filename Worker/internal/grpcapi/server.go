// Package grpcapi implements the Worker side of the OpsGaurd management gRPC
// service (proto/opsguard.proto). It is a thin adapter over the existing
// orchestrator / monitor / audit / nodeagent logic — the HTTP management API
// and this gRPC service expose the same capabilities, just over different
// transports. The node-level local API (nodeagent stats/exec/host) stays on
// HTTP and is reached via the orchestrator's NodeClient.
package grpcapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/audit"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/grpcapi/pb"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/monitor"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/nodeagent"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/orchestrator"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Server implements pb.ManagementServiceServer. It holds the backends; each
// handler delegates to the orchestrator/monitor/audit/nodeagent layers exactly
// as the HTTP API did.
type Server struct {
	pb.UnimplementedManagementServiceServer

	orch   *orchestrator.Orchestrator
	events *monitor.EventStore
	audit  *audit.Store
	local  *nodeagent.API
	log    *slog.Logger

	// leader forwarding: an internal gRPC client to the swarm leader's
	// management service, used by non-leader managers for write RPCs. Cached
	// per leader address; rebuilt when leadership changes.
	leaderMu    sync.Mutex
	leaderAddr  string // cached leader host:port
	leaderConn  *grpc.ClientConn
	leaderToken string // token forwarded from the incoming request's metadata
}

// New creates a management gRPC server. All backends are required for a
// manager-role worker. local is the nodeagent.API used for NodeStats and
// StreamLogs on this node.
func New(orch *orchestrator.Orchestrator, events *monitor.EventStore, auditStore *audit.Store, local *nodeagent.API, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{orch: orch, events: events, audit: auditStore, local: local, log: log}
}

// Register attaches the service to a *grpc.Server (called from main).
func (s *Server) Register(srv *grpc.Server) {
	pb.RegisterManagementServiceServer(srv, s)
}

// ---- liveness ----

func (s *Server) Ping(ctx context.Context, _ *pb.Empty) (*pb.Pong, error) {
	return &pb.Pong{Status: "ok"}, nil
}

func (s *Server) Self(ctx context.Context, _ *pb.Empty) (*pb.SelfInfo, error) {
	si, err := s.orch.Self(ctx)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	return &pb.SelfInfo{
		NodeId:       si.NodeID,
		Hostname:     si.Hostname,
		Role:         si.Role,
		Leader:       si.Leader,
		State:        si.State,
		SwarmManager: si.SwarmManager,
		Addr:         si.Addr,
	}, nil
}

// ---- workload read ----

func (s *Server) ListServices(ctx context.Context, req *pb.ListServicesRequest) (*pb.ListServicesResponse, error) {
	svcs, err := s.orch.ListServices(ctx, req.GetLabel())
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	raw, err := json.Marshal(svcs)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pb.ListServicesResponse{ServicesJson: raw}, nil
}

func (s *Server) GetService(ctx context.Context, req *pb.GetServiceRequest) (*pb.ServiceDetail, error) {
	d, err := s.orch.Inspect(ctx, req.GetName())
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	svcJSON, _ := json.Marshal(d.Service)
	tasksJSON, _ := json.Marshal(d.Tasks)
	return &pb.ServiceDetail{
		ServiceJson: svcJSON,
		TasksJson:   tasksJSON,
		Running:     int32(d.Running),
		Desired:     int32(d.Desired),
		Healthy:     int32(d.Healthy),
	}, nil
}

// ---- workload write (with leader forwarding) ----

// leaderClient returns an internal gRPC client to the swarm leader's
// management service, used to forward write RPCs when this instance is not the
// leader. The connection is cached per leader address and rebuilt on leadership
// changes. The incoming bearer token is propagated so the leader authenticates
// the forwarded call with the same actor. Returns (client, true, nil) when
// forwarding applies; (nil, false, nil) when this instance IS the leader (or
// leadership is unknown) so the caller handles the RPC locally.
func (s *Server) leaderClient(ctx context.Context) (pb.ManagementServiceClient, bool, error) {
	leader, err := s.orch.AmILeader(ctx)
	if err != nil || leader {
		return nil, false, nil // we are leader (or unknown): handle locally
	}
	addr, err := s.orch.LeaderGRPCAddr(ctx)
	if err != nil {
		return nil, true, status.Error(codes.Unavailable, "resolve leader: "+err.Error())
	}
	// Propagate the incoming token to the leader.
	token := tokenFromContext(ctx)

	s.leaderMu.Lock()
	defer s.leaderMu.Unlock()
	if s.leaderConn != nil && s.leaderAddr == addr {
		return pb.NewManagementServiceClient(s.leaderConn), true, nil
	}
	// Leadership changed (or first call): rebuild the connection.
	if s.leaderConn != nil {
		_ = s.leaderConn.Close()
		s.leaderConn = nil
	}
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, true, status.Error(codes.Unavailable, "dial leader "+addr+": "+err.Error())
	}
	s.leaderConn = conn
	s.leaderAddr = addr
	s.leaderToken = token
	return pb.NewManagementServiceClient(conn), true, nil
}

// Close releases the cached leader-forwarding connection (called on shutdown).
func (s *Server) Close() error {
	s.leaderMu.Lock()
	defer s.leaderMu.Unlock()
	if s.leaderConn != nil {
		err := s.leaderConn.Close()
		s.leaderConn = nil
		return err
	}
	return nil
}

// tokenFromContext extracts the bearer token from incoming gRPC metadata.
func tokenFromContext(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	for _, v := range md.Get("authorization") {
		if t := strings.TrimPrefix(v, "Bearer "); t != v {
			return t
		}
	}
	return ""
}

// forwardCtx returns ctx with the leader token attached as outgoing metadata.
func (s *Server) forwardCtx(ctx context.Context) context.Context {
	if s.leaderToken != "" {
		return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+s.leaderToken)
	}
	return ctx
}

func (s *Server) Deploy(ctx context.Context, req *pb.DeployRequest) (*pb.Operation, error) {
	if lc, fwd, err := s.leaderClient(ctx); fwd {
		if err != nil {
			return nil, err
		}
		op, ferr := lc.Deploy(s.forwardCtx(ctx), req)
		return op, wrapLeaderErr(ferr)
	}
	cfg, err := decodeConfig(req.GetConfigBody(), req.GetIsJson())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	op, err := s.orch.Deploy(ctx, cfg)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return opSnap(op), nil
}

func (s *Server) Update(ctx context.Context, req *pb.UpdateRequest) (*pb.Operation, error) {
	if lc, fwd, err := s.leaderClient(ctx); fwd {
		if err != nil {
			return nil, err
		}
		op, ferr := lc.Update(s.forwardCtx(ctx), req)
		return op, wrapLeaderErr(ferr)
	}
	cfg, err := decodeConfig(req.GetConfigBody(), req.GetIsJson())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	op, err := s.orch.Update(ctx, req.GetName(), cfg)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return opSnap(op), nil
}

func (s *Server) Scale(ctx context.Context, req *pb.ScaleRequest) (*pb.Operation, error) {
	if lc, fwd, err := s.leaderClient(ctx); fwd {
		if err != nil {
			return nil, err
		}
		op, ferr := lc.Scale(s.forwardCtx(ctx), req)
		return op, wrapLeaderErr(ferr)
	}
	op, err := s.orch.Scale(ctx, req.GetName(), req.GetReplicas())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return opSnap(op), nil
}

func (s *Server) Restart(ctx context.Context, req *pb.RestartRequest) (*pb.Operation, error) {
	if lc, fwd, err := s.leaderClient(ctx); fwd {
		if err != nil {
			return nil, err
		}
		op, ferr := lc.Restart(s.forwardCtx(ctx), req)
		return op, wrapLeaderErr(ferr)
	}
	op, err := s.orch.Restart(ctx, req.GetName())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return opSnap(op), nil
}

func (s *Server) Remove(ctx context.Context, req *pb.RemoveRequest) (*pb.Operation, error) {
	if lc, fwd, err := s.leaderClient(ctx); fwd {
		if err != nil {
			return nil, err
		}
		op, ferr := lc.Remove(s.forwardCtx(ctx), req)
		return op, wrapLeaderErr(ferr)
	}
	op, err := s.orch.Remove(ctx, req.GetName())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return opSnap(op), nil
}

func (s *Server) GetOperation(ctx context.Context, req *pb.GetOperationRequest) (*pb.Operation, error) {
	op, ok := s.orch.GetOperation(req.GetId())
	if !ok {
		return nil, status.Error(codes.NotFound, "operation "+req.GetId()+" not found")
	}
	return opSnap(&op), nil
}

// ---- nodes / host ----

func (s *Server) ListNodes(ctx context.Context, _ *pb.Empty) (*pb.ListNodesResponse, error) {
	views, err := s.orch.ListNodesView(ctx)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	nodes := make([]*pb.Node, 0, len(views))
	for _, v := range views {
		nodes = append(nodes, &pb.Node{
			Id:             v.ID,
			Hostname:       v.Hostname,
			Role:           v.Role,
			State:          v.State,
			Availability:   v.Availability,
			Addr:           v.Addr,
			Leader:         v.Leader,
			ManagerReach:   v.ManagerReach,
			Reachable:      v.Reachable,
			CpuCores:       v.CPUCores,
			MemBytes:       v.MemBytes,
			CpuPercent:     v.CPUPercent,
			MemPercent:     v.MemPercent,
			ContainerCount: int32(v.ContainerCount),
		})
	}
	return &pb.ListNodesResponse{Nodes: nodes}, nil
}

func (s *Server) NodeStats(ctx context.Context, _ *pb.Empty) (*pb.NodeStatsResponse, error) {
	// NodeStats reads the LOCAL node's container stats via nodeagent (same
	// collection logic as GET /api/v1/local/stats).
	stats, err := s.local.LocalStats(ctx)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	resp := &pb.NodeStatsResponse{Node: stats.Node}
	for _, c := range stats.Containers {
		resp.Containers = append(resp.Containers, &pb.ContainerStat{
			ContainerId: c.ContainerID,
			Service:     c.Service,
			TaskId:      c.TaskID,
			CpuPercent:  c.CPUPercent,
			MemPercent:  c.MemPercent,
			MemUsage:    c.MemUsage,
			MemLimit:    c.MemLimit,
		})
	}
	return resp, nil
}

func (s *Server) NodeProcesses(ctx context.Context, req *pb.NodeProcessesRequest) (*pb.ProcessesResponse, error) {
	addr, err := s.orch.ResolveNodeAddr(ctx, req.GetNodeId())
	if err != nil {
		if orchestrator.NodeNotFound(err) {
			return nil, status.Error(codes.NotFound, err.Error())
		}
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	procs, err := s.orch.NodeClientByAddr(addr).Processes(ctx, req.GetTop(), fmt.Sprintf("%d", req.GetLimit()), req.GetFilter())
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	resp := &pb.ProcessesResponse{Node: procs.Node, Total: int32(procs.Total)}
	for _, p := range procs.Processes {
		resp.Processes = append(resp.Processes, &pb.ProcessInfo{
			Pid:        int32(p.PID),
			Name:       p.Name,
			Cmdline:    p.Cmdline,
			State:      p.State,
			MemKb:      p.MemKB,
			CpuPercent: p.CPUPercent,
		})
	}
	return resp, nil
}

// ---- probes ----

func (s *Server) CheckPort(ctx context.Context, req *pb.CheckPortRequest) (*pb.PortCheckResult, error) {
	addr, err := s.orch.ResolveNodeAddr(ctx, req.GetNodeId())
	if err != nil {
		if orchestrator.NodeNotFound(err) {
			return nil, status.Error(codes.NotFound, err.Error())
		}
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	res, err := s.orch.NodeClientByAddr(addr).CheckPort(ctx, req.GetHost(), fmt.Sprintf("%d", req.GetPort()), req.GetTimeout())
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	return &pb.PortCheckResult{
		Node: res.Node, Host: res.Host, Port: res.Port, Ok: res.OK,
		LatencyMs: res.LatencyMS, Error: res.Error,
	}, nil
}

func (s *Server) CheckHTTP(ctx context.Context, req *pb.CheckHTTPRequest) (*pb.HTTPCheckResult, error) {
	addr, err := s.orch.ResolveNodeAddr(ctx, req.GetNodeId())
	if err != nil {
		if orchestrator.NodeNotFound(err) {
			return nil, status.Error(codes.NotFound, err.Error())
		}
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	httpReq := orchestrator.HTTPCheckRequest{
		URL:            req.GetUrl(),
		Method:         req.GetMethod(),
		Headers:        req.GetHeaders(),
		ExpectedStatus: ints32ToInts(req.GetExpectedStatus()),
		ExpectedBody:   req.GetExpectedBody(),
		Timeout:        req.GetTimeout(),
	}
	res, err := s.orch.NodeClientByAddr(addr).CheckHTTP(ctx, httpReq)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	return &pb.HTTPCheckResult{
		Node: res.Node, Url: res.URL, Ok: res.OK, Status: int32(res.Status),
		LatencyMs: res.LatencyMS, Error: res.Error,
	}, nil
}

func (s *Server) CheckFlow(ctx context.Context, req *pb.CheckFlowRequest) (*pb.FlowCheckResult, error) {
	addr, err := s.orch.ResolveNodeAddr(ctx, req.GetNodeId())
	if err != nil {
		if orchestrator.NodeNotFound(err) {
			return nil, status.Error(codes.NotFound, err.Error())
		}
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	var steps []orchestrator.FlowStep
	for _, st := range req.GetSteps() {
		steps = append(steps, orchestrator.FlowStep{
			Name: st.GetName(), URL: st.GetUrl(), Method: st.GetMethod(),
			Headers: st.GetHeaders(), Body: st.GetBody(),
			ExpectStatus: ints32ToInts(st.GetExpectStatus()), ExpectBody: st.GetExpectBody(),
			Extract: st.GetExtract(),
		})
	}
	flowReq := orchestrator.FlowCheckRequest{Steps: steps, Vars: req.GetVars(), Timeout: req.GetTimeout()}
	res, err := s.orch.NodeClientByAddr(addr).CheckFlow(ctx, flowReq)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	out := &pb.FlowCheckResult{
		Node: res.Node, Ok: res.OK, FailedStep: res.FailedStep,
		LatencyMs: res.LatencyMS, Error: res.Error,
	}
	for _, sr := range res.Steps {
		out.Steps = append(out.Steps, &pb.FlowStepResult{
			Name: sr.Name, Ok: sr.OK, Status: int32(sr.Status),
			LatencyMs: sr.LatencyMS, Extracted: sr.Extracted, Error: sr.Error,
		})
	}
	return out, nil
}

// ---- events / audit point queries ----

func (s *Server) ListEvents(ctx context.Context, req *pb.ListEventsRequest) (*pb.ListEventsResponse, error) {
	evs := s.events.List(req.GetService(), monitor.EventType(req.GetType()), int(req.GetLimit()))
	raw, err := json.Marshal(evs)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pb.ListEventsResponse{EventsJson: raw}, nil
}

func (s *Server) ListAudit(ctx context.Context, req *pb.ListAuditRequest) (*pb.ListAuditResponse, error) {
	entries := s.audit.List(audit.Action(req.GetAction()), int(req.GetLimit()))
	raw, err := json.Marshal(entries)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pb.ListAuditResponse{EntriesJson: raw}, nil
}

// ---- helpers ----

// decodeConfig parses a YAML/JSON config body into a *config.Config, mirroring
// the HTTP decodeConfig helper. isJson selects the format hint.
func decodeConfig(body []byte, isJson bool) (*config.Config, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("config body is empty")
	}
	name := "deploy.yaml"
	if isJson {
		name = "deploy.json"
	}
	return config.LoadBytes(name, body)
}

// opSnap converts an orchestrator Operation into the protobuf Operation. The
// orchestrator returns a snapshot-safe copy via GetOperation; Deploy etc.
// return pointers to live objects guarded by an internal mutex, so we read
// through the snapshot path.
func opSnap(op *orchestrator.Operation) *pb.Operation {
	if op == nil {
		return nil
	}
	// Use the store's snapshot mechanism to get a concurrency-safe copy.
	snap := orchestrator.SnapshotOperation(op)
	out := &pb.Operation{
		Id:        snap.ID,
		Type:      string(snap.Type),
		Service:   snap.Service,
		Status:    string(snap.Status),
		StartedAt: snap.StartedAt.Format(time.RFC3339Nano),
		Steps:     snap.Steps,
		Replicas:  snap.Replicas,
		Mode:      snap.Mode,
		ServiceId: snap.ServiceID,
		Error:     snap.Error,
	}
	if snap.FinishedAt != nil {
		out.FinishedAt = snap.FinishedAt.Format(time.RFC3339Nano)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// wrapLeaderErr converts a leader-forwarding gRPC error. A nil error passes
// through; gRPC status errors propagate as-is (preserving the leader's code);
// non-status errors become Unavailable (the leader connection broke).
func wrapLeaderErr(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Error(codes.Unavailable, "leader forward: "+err.Error())
}

// ints32ToInts converts a protobuf int32 slice to a Go int slice (Docker check
// types use []int for status codes).
func ints32ToInts(in []int32) []int {
	if len(in) == 0 {
		return nil
	}
	out := make([]int, len(in))
	for i, v := range in {
		out[i] = int(v)
	}
	return out
}
