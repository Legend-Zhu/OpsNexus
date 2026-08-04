package docker

import (
	"context"
	"fmt"
	"io"
)

// ExecCreateOptions is the body for POST /containers/{id}/exec.
type ExecCreateOptions struct {
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	DetachKeys   string   `json:"DetachKeys,omitempty"`
	Tty          bool     `json:"Tty"`
	Cmd          []string `json:"Cmd"`
}

type execCreateResp struct {
	ID string `json:"Id"`
}

// ExecStartOptions is the body for POST /exec/{id}/start.
type ExecStartOptions struct {
	Detach bool `json:"Detach"`
	Tty    bool `json:"Tty"`
}

// ExecInspect is the subset of GET /exec/{id}/json Worker uses.
type ExecInspect struct {
	Running  bool `json:"Running"`
	ExitCode int  `json:"ExitCode"`
}

// ContainerExecCreate creates an exec instance inside a running container and
// returns its ID.
func (c *httpClient) ContainerExecCreate(ctx context.Context, containerID string, cmd []string) (string, error) {
	body := ExecCreateOptions{
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          cmd,
	}
	var resp execCreateResp
	if err := c.postJSON(ctx, "/containers/"+containerID+"/exec", nil, body, &resp, nil); err != nil {
		return "", fmt.Errorf("exec create: %w", err)
	}
	return resp.ID, nil
}

// ExecStart starts the exec instance and returns a stream of the merged
// stdout/stderr output (raw bytes, no framing — unlike container logs). The
// caller must close the reader.
func (c *httpClient) ExecStart(ctx context.Context, execID string) (io.ReadCloser, error) {
	body := ExecStartOptions{Detach: false, Tty: false}
	rc, err := c.postJSONStream(ctx, "/exec/"+execID+"/start", body)
	if err != nil {
		return nil, fmt.Errorf("exec start: %w", err)
	}
	return rc, nil
}

// ExecInspect returns the exit code of a finished exec instance.
func (c *httpClient) ExecInspect(ctx context.Context, execID string) (ExecInspect, error) {
	var ei ExecInspect
	if err := c.getJSON(ctx, "/exec/"+execID+"/json", nil, &ei); err != nil {
		return ExecInspect{}, fmt.Errorf("exec inspect: %w", err)
	}
	return ei, nil
}
