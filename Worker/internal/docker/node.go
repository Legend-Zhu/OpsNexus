package docker

import (
	"context"
	"fmt"
)

// ListNodes lists swarm nodes, optionally filtered.
func (c *httpClient) ListNodes(ctx context.Context, f Filter) ([]Node, error) {
	q := filtersQuery(f)
	var nodes []Node
	if err := c.getJSON(ctx, "/nodes", q, &nodes); err != nil {
		return nil, err
	}
	return nodes, nil
}

// SelfNode finds the local node by matching the OS hostname against each
// node's Description.Hostname. Swarm has no "get self" API, so hostname match
// is the standard approach; the Worker must run with hostname == swarm node
// hostname (the default when the container joins the swarm).
func (c *httpClient) SelfNode(ctx context.Context) (Node, error) {
	hostname, err := localHostname()
	if err != nil {
		return Node{}, err
	}
	nodes, err := c.ListNodes(ctx, nil)
	if err != nil {
		return Node{}, err
	}
	for _, n := range nodes {
		if n.Description.Hostname == hostname {
			return n, nil
		}
	}
	return Node{}, fmt.Errorf("no swarm node matching local hostname %q (is this host part of the swarm?)", hostname)
}
