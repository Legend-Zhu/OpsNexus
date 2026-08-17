// Batch container health lookup for cross-node healthy aggregation: the
// manager fan-outs one POST per node that hosts running tasks of a service,
// instead of one round trip per container.
package nodeagent

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
)

type containersHealthReq struct {
	IDs []string `json:"ids"`
}

// containersHealthResp is mirrored by orchestrator.ContainerHealthResp.
type containersHealthResp struct {
	Node   string            `json:"node"`
	Health map[string]string `json:"health"` // containerID -> healthy|unhealthy|starting
}

// containersHealth POST /api/v1/local/containers/health
// 批量查询本机容器健康状态（供 manager 做跨节点 healthy 汇聚）。Health 为 nil
// （容器未配置 healthcheck）或查询失败的容器不出现在 map 中，调用方将缺失一律
// 按不健康计。
func (a *API) containersHealth(w http.ResponseWriter, r *http.Request) {
	var req containersHealthReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(req.IDs) == 0 {
		writeJSON(w, http.StatusOK, containersHealthResp{Node: hostnameOr(""), Health: map[string]string{}})
		return
	}
	writeJSON(w, http.StatusOK, containersHealthResp{Node: hostnameOr(""), Health: a.ContainersHealth(r.Context(), req.IDs)})
}

// ContainersHealth inspects the given containers concurrently (each index is
// written by at most one goroutine, mirroring LocalStats) and returns the
// health status of those that actually report one. Per-id failures are
// skipped silently: from the caller's side a missing id is indistinguishable
// from a non-healthy container, and both are counted as not healthy.
func (a *API) ContainersHealth(ctx context.Context, ids []string) map[string]string {
	health := make([]string, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		if id == "" {
			continue
		}
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			ci, err := a.cli.ContainerInspect(ctx, id)
			if err == nil && ci.State.Health != nil {
				health[i] = ci.State.Health.Status
			}
		}(i, id)
	}
	wg.Wait()
	out := make(map[string]string, len(ids))
	for i, id := range ids {
		if health[i] != "" {
			out[id] = health[i]
		}
	}
	return out
}
