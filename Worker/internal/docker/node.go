package docker

import (
	"context"
	"fmt"
)

// Info fetches the engine /info subset Worker needs.
func (c *httpClient) Info(ctx context.Context) (Info, error) {
	var info Info
	if err := c.getJSON(ctx, "/info", nil, &info); err != nil {
		return Info{}, err
	}
	return info, nil
}

// ListNodes lists swarm nodes, optionally filtered.
func (c *httpClient) ListNodes(ctx context.Context, f Filter) ([]Node, error) {
	q := filtersQuery(f)
	var nodes []Node
	if err := c.getJSON(ctx, "/nodes", q, &nodes); err != nil {
		return nil, err
	}
	return nodes, nil
}

// SelfNode identifies the local node via the engine's own Swarm.NodeID
// (from /info), which is authoritative and does not rely on hostname matching.
func (c *httpClient) SelfNode(ctx context.Context) (Node, error) {
	info, err := c.Info(ctx)
	if err != nil {
		return Node{}, err
	}
	if info.Swarm.NodeID == "" {
		return Node{}, fmt.Errorf("this daemon is not part of a swarm (local node state %q)", info.Swarm.LocalNodeState)
	}
	nodes, err := c.ListNodes(ctx, nil)
	if err != nil {
		return Node{}, err
	}
	for _, n := range nodes {
		if n.ID == info.Swarm.NodeID {
			return n, nil
		}
	}
	return Node{}, fmt.Errorf("swarm node %q not found in node list", info.Swarm.NodeID)
}
