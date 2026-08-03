package docker

import (
	"context"
	"net/url"
	"strconv"
)

// ServiceCreate creates a swarm service and returns its ID. registryAuth, when
// non-empty, is sent as the X-Registry-Auth header (base64 of a
// ~/.docker/config.json) so private images can be pulled by the manager.
func (c *httpClient) ServiceCreate(ctx context.Context, spec ServiceSpec, registryAuth string) (string, error) {
	var resp serviceCreateResponse
	headers := authHeader(registryAuth)
	if err := c.postJSON(ctx, "/services/create", nil, spec, &resp, headers); err != nil {
		return "", err
	}
	return resp.ID, nil
}

// ServiceUpdate replaces a service's spec. It fetches the current Version
// (optimistic concurrency) and applies the new spec.
func (c *httpClient) ServiceUpdate(ctx context.Context, id string, spec ServiceSpec, registryAuth string) error {
	svc, err := c.GetService(ctx, id)
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Set("version", strconv.FormatUint(svc.Version.Index, 10))
	return c.postJSON(ctx, "/services/"+id+"/update", q, spec, nil, authHeader(registryAuth))
}

// ServiceRemove deletes a service. Missing services are not an error.
func (c *httpClient) ServiceRemove(ctx context.Context, id string) error {
	return c.del(ctx, "/services/"+id, nil)
}

// ServiceScale sets the replica count of a replicated service. The caller is
// responsible for not scaling global services.
func (c *httpClient) ServiceScale(ctx context.Context, id string, replicas uint64) error {
	svc, err := c.GetService(ctx, id)
	if err != nil {
		return err
	}
	spec := svc.Spec
	spec.Mode = ServiceMode{Replicated: &ReplicatedService{Replicas: &replicas}}
	// Scale does not pull new images, so no registry auth header.
	return c.ServiceUpdate(ctx, id, spec, "")
}

// ServiceRestart forces swarm to re-create the service's tasks (the engine
// equivalent of `docker service update --force`): it fetches the current spec,
// increments ForceUpdate, and re-applies it.
func (c *httpClient) ServiceRestart(ctx context.Context, id string) error {
	svc, err := c.GetService(ctx, id)
	if err != nil {
		return err
	}
	spec := svc.Spec
	spec.TaskTemplate.ForceUpdate = svc.Spec.TaskTemplate.ForceUpdate + 1
	q := url.Values{}
	q.Set("version", strconv.FormatUint(svc.Version.Index, 10))
	return c.postJSON(ctx, "/services/"+id+"/update", q, spec, nil, nil)
}

// ListServices lists services with running/desired task counts (status=true).
// Pass nil to list all.
func (c *httpClient) ListServices(ctx context.Context, f Filter) ([]Service, error) {
	q := filtersQuery(f)
	q.Set("status", "true")
	var svcs []Service
	if err := c.getJSON(ctx, "/services", q, &svcs); err != nil {
		return nil, err
	}
	return svcs, nil
}

// GetService inspects a single service by ID or name.
func (c *httpClient) GetService(ctx context.Context, id string) (Service, error) {
	var svc Service
	if err := c.getJSON(ctx, "/services/"+id, nil, &svc); err != nil {
		return Service{}, err
	}
	return svc, nil
}
