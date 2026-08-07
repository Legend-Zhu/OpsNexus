package mcp

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerResources exposes read-only views of the cluster as MCP Resources.
func (h *Handler) registerResources(s *mcp.Server) {
	// worker://services — service list
	s.AddResource(&mcp.Resource{
		URI:         "worker://services",
		Name:        "Services",
		Description: "List of swarm services with replica counts",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		svcs, err := h.orch.ListServices(ctx, "")
		if err != nil {
			return nil, err
		}
		return textResult(req, svcs), nil
	})

	// worker://services/{name} — service detail
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "worker://services/{name}",
		Name:        "Service detail",
		Description: "Full detail of a service (spec, tasks, health)",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		uri := req.Params.URI
		prefix := "worker://services/"
		if !strings.HasPrefix(uri, prefix) {
			return nil, fmt.Errorf("unexpected template URI %q", uri)
		}
		name := strings.TrimPrefix(uri, prefix)
		if name == "" {
			return nil, fmt.Errorf("missing service name in URI %q", uri)
		}
		d, err := h.orch.Inspect(ctx, name)
		if err != nil {
			return nil, err
		}
		return textResult(req, d), nil
	})

	// worker://events — recent events
	s.AddResource(&mcp.Resource{
		URI:         "worker://events",
		Name:        "Monitoring events",
		Description: "Recent monitoring events (newest first)",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		evs := h.mon.Events("", "", 0, 50)
		return textResult(req, evs), nil
	})
}

// textResult wraps any JSON-marshalable value into a ReadResourceResult.
func textResult(req *mcp.ReadResourceRequest, v any) *mcp.ReadResourceResult {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(fmt.Sprintf("{\"error\":%q}", err.Error()))
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{URI: req.Params.URI, MIMEType: "application/json", Text: string(b)},
		},
	}
}

// recentLogs reads the last N log lines of a service (aggregated across tasks).
func (h *Handler) recentLogs(ctx context.Context, service string, n int) ([]string, error) {
	rc, err := h.cli.ServiceLogs(ctx, service, docker.LogsOptions{
		Stdout:     true,
		Stderr:     true,
		Timestamps: false,
		Tail:       fmt.Sprintf("%d", n),
	})
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	lines := make([]string, 0, n)
	var pending []byte
	emit := func() {
		if len(pending) > 0 {
			lines = append(lines, strings.TrimRight(string(pending), "\r"))
			pending = pending[:0]
		}
	}
	for {
		var hdr [8]byte
		if _, err := io.ReadFull(rc, hdr[:]); err != nil {
			emit()
			break
		}
		size := binary.BigEndian.Uint32(hdr[4:8])
		buf := make([]byte, size)
		if _, err := io.ReadFull(rc, buf); err != nil {
			emit()
			break
		}
		for _, b := range buf {
			if b == '\n' {
				emit()
			} else {
				pending = append(pending, b)
			}
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}
