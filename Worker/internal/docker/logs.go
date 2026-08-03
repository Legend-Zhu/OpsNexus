package docker

import (
	"context"
	"io"
	"net/url"
)

// ServiceLogs opens a stream of service log lines. The caller must close the
// returned reader. Only json-file / journald log drivers are supported by the
// engine. If neither Stdout nor Stderr is set, both default to true.
func (c *httpClient) ServiceLogs(ctx context.Context, serviceID string, opts LogsOptions) (io.ReadCloser, error) {
	if !opts.Stdout && !opts.Stderr {
		opts.Stdout = true
		opts.Stderr = true
	}
	q := url.Values{}
	if opts.Follow {
		q.Set("follow", "1")
	}
	if opts.Stdout {
		q.Set("stdout", "1")
	}
	if opts.Stderr {
		q.Set("stderr", "1")
	}
	if opts.Timestamps {
		q.Set("timestamps", "1")
	}
	if opts.Tail != "" {
		q.Set("tail", opts.Tail)
	}
	if opts.Since != "" {
		q.Set("since", opts.Since)
	}
	if opts.Details {
		q.Set("details", "1")
	}
	return c.getStream(ctx, "/services/"+serviceID+"/logs", q)
}
