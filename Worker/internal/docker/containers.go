// Container-level operations (docker restart etc.) beyond read views.
package docker

import (
	"context"
	"net/http"
)

// RestartContainer restarts a container by ID or name (POST /containers/{id}/restart).
// The Docker Engine API default timeout is used (no timeout query param).
func (c *httpClient) RestartContainer(ctx context.Context, containerID string) error {
	data, code, err := c.do(ctx, http.MethodPost, "/containers/"+containerID+"/restart", nil, nil, nil)
	if err != nil {
		return err
	}
	if code >= 400 {
		return apiErrorFrom(code, data)
	}
	return nil
}
