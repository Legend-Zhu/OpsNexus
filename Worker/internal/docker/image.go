package docker

import (
	"context"
	"io"
	"net/url"
)

// authHeader builds the X-Registry-Auth header map, or nil when auth is empty
// (public registry; omitting the header avoids confusing the daemon).
func authHeader(registryAuth string) map[string]string {
	if registryAuth == "" {
		return nil
	}
	return map[string]string{"X-Registry-Auth": registryAuth}
}

// ImagePull pulls an image explicitly (used for imagePullPolicy=always and for
// private registries where the manager must have the image before tasks start
// pulling). The registryAuth value is the base64 of a ~/.docker/config.json.
func (c *httpClient) ImagePull(ctx context.Context, ref, registryAuth string) error {
	q := url.Values{}
	q.Set("fromImage", ref)
	rc, err := c.postRaw(ctx, "/images/create", q, authHeader(registryAuth))
	if err != nil {
		return err
	}
	defer rc.Close()
	// The response is a stream of JSON progress messages; drain it so the pull
	// completes before we return.
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return err
	}
	return nil
}

// GetSecret fetches a swarm secret by name or id. Used to resolve
// registryAuth.secretRef: the secret's Data is the base64 of a
// ~/.docker/config.json.
func (c *httpClient) GetSecret(ctx context.Context, name string) (Secret, error) {
	var s Secret
	if err := c.getJSON(ctx, "/secrets/"+name, nil, &s); err != nil {
		return Secret{}, err
	}
	return s, nil
}
