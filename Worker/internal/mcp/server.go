// Package mcp exposes the Worker's orchestration and monitoring capabilities
// over MCP (Model Context Protocol), spec version 2026-07-28, so LLM agents
// can deploy, inspect, and operate swarm services. It builds on the official
// go-sdk (github.com/modelcontextprotocol/go-sdk), which implements the
// stateless 2026-07-28 model: no initialize handshake, per-request capability
// declaration, server/discover, and Streamable HTTP transport.
package mcp

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/audit"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/monitor"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/orchestrator"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/version"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Handler bundles the MCP server with its backends (orchestrator for
// lifecycle, monitor for events, docker client for nodes/logs).
type Handler struct {
	srv   *mcp.Server
	orch  *orchestrator.Orchestrator
	mon   *monitor.Manager
	cli   docker.Client
	log   *slog.Logger
	audit *audit.Store
}

// New constructs the MCP server and registers all tools and resources.
func New(orch *orchestrator.Orchestrator, mon *monitor.Manager, cli docker.Client, log *slog.Logger) (*Handler, error) {
	return NewWithAudit(orch, mon, cli, log, nil)
}

// NewWithAudit builds the server with an optional audit store.
func NewWithAudit(orch *orchestrator.Orchestrator, mon *monitor.Manager, cli docker.Client, log *slog.Logger, auditStore *audit.Store) (*Handler, error) {
	if log == nil {
		log = slog.Default()
	}
	h := &Handler{orch: orch, mon: mon, cli: cli, log: log, audit: auditStore}

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "opsguard-worker",
		Title:   "OpsGaurd Worker",
		Version: version.Version,
	}, &mcp.ServerOptions{
		Instructions: "OpsGaurd Worker MCP server. Deploy and operate Docker Swarm services, " +
			"and query health events. Destructive operations (remove_service, " +
			"scale_service to 0) require confirm=true.",
		Logger: log,
	})

	h.registerTools(srv)
	h.registerResources(srv)
	h.registerToolsMetrics(srv)
	h.registerToolsChecks(srv)
	h.srv = srv
	return h, nil
}

// HTTPHandler returns a net/http handler implementing MCP Streamable HTTP
// (single POST endpoint, per-request stateless handling, SSE responses).
// Stateless is required for the 2026-07-28 protocol version (no sessions).
func (h *Handler) HTTPHandler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		// Stateless: every request gets a fresh server instance (schema cache
		// optional; P3 keeps it simple).
		return h.srv
	}, &mcp.StreamableHTTPOptions{Stateless: true})
}

// ServeStdio runs the MCP server over stdin/stdout (newline-delimited
// JSON-RPC; local agents, debugging). Blocks until ctx is cancelled or the
// stream ends. There is no HTTP caller to authenticate, so stdio sessions
// are tagged with the fixed actor "stdio" in the audit log.
func (h *Handler) ServeStdio(ctx context.Context) error {
	tr := &stdioTransport{r: os.Stdin, w: os.Stdout}
	return h.srv.Run(audit.ContextWithActor(ctx, "stdio"), tr)
}

// stdioTransport implements mcp.Transport over an io.Reader/io.Writer with
// newline-delimited JSON-RPC framing, per the 2026-07-28 spec's stdio
// binding (same framing as the SDK's custom-transport example).
type stdioTransport struct {
	r io.Reader
	w io.Writer
}

func (t *stdioTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	return &stdioConn{r: bufio.NewReader(t.r), w: t.w}, nil
}

type stdioConn struct {
	r *bufio.Reader
	w io.Writer
}

func (c *stdioConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	data, err := c.r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return jsonrpc.DecodeMessage(data[:len(data)-1])
}

func (c *stdioConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	data, err := jsonrpc.EncodeMessage(msg)
	if err != nil {
		return err
	}
	if _, err := c.w.Write(data); err != nil {
		return err
	}
	_, err = c.w.Write([]byte{'\n'})
	return err
}

func (c *stdioConn) Close() error { return nil }

func (c *stdioConn) SessionID() string { return "" }
