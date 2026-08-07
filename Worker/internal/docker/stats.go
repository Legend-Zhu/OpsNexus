package docker

import (
	"context"
	"net/url"
)

// ListContainers lists containers matching the given filter. The monitor uses
// label "com.docker.swarm.service.id=<id>" to find a service's task containers.
func (c *httpClient) ListContainers(ctx context.Context, f Filter) ([]Container, error) {
	q := filtersQuery(f)
	var cs []Container
	if err := c.getJSON(ctx, "/containers/json", q, &cs); err != nil {
		return nil, err
	}
	return cs, nil
}

// ListAllContainers lists every container on the node (running and exited),
// regardless of swarm membership — docker ps -a equivalent.
func (c *httpClient) ListAllContainers(ctx context.Context) ([]Container, error) {
	q := url.Values{}
	q.Set("all", "1")
	var cs []Container
	if err := c.getJSON(ctx, "/containers/json", q, &cs); err != nil {
		return nil, err
	}
	return cs, nil
}

// ContainerStats takes a one-shot stats snapshot for a container.
func (c *httpClient) ContainerStats(ctx context.Context, containerID string) (Stats, error) {
	q := url.Values{}
	q.Set("stream", "false")
	var s Stats
	if err := c.getJSON(ctx, "/containers/"+containerID+"/stats", q, &s); err != nil {
		return Stats{}, err
	}
	return s, nil
}

// ContainerInspect returns the container's state (incl. Health.Status) for
// readiness judgment. Used when a service has a healthcheck configured.
func (c *httpClient) ContainerInspect(ctx context.Context, containerID string) (ContainerInspect, error) {
	var ci ContainerInspect
	if err := c.getJSON(ctx, "/containers/"+containerID+"/json", nil, &ci); err != nil {
		return ContainerInspect{}, err
	}
	return ci, nil
}
