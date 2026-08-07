// Node container inventory: every container on the node (swarm service tasks
// AND standalone `docker run` containers such as r-nacos, grafana, nginxwebui).
// Standalone containers are invisible to the swarm-service views, so the node
// drawer lists them here.
package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
)

// ContainerInfo mirrors proto.ContainerInfo (kept here as the single source of
// truth shared by the HTTP endpoint and the gRPC NodeContainers RPC).
type ContainerInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	State   string `json:"state"`
	Type    string `json:"type"`    // service | standalone
	Service string `json:"service"` // swarm service name when type=service
	Ports   string `json:"ports"`   // "80->8080/tcp, 443->8443/tcp"
}

// LocalContainers lists all containers on this node (docker ps -a equivalent).
func (a *API) LocalContainers(ctx context.Context) ([]ContainerInfo, error) {
	cs, err := a.cli.ListAllContainers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ContainerInfo, 0, len(cs))
	for _, c := range cs {
		ci := ContainerInfo{
			ID:    c.ID,
			Image: c.Image,
			State: c.State,
		}
		if len(c.Names) > 0 {
			ci.Name = strings.TrimPrefix(c.Names[0], "/")
		}
		if _, ok := c.Labels["com.docker.swarm.service.id"]; ok {
			ci.Type = "service"
			ci.Service = c.Labels["com.docker.swarm.service.name"]
		} else {
			ci.Type = "standalone"
		}
		ci.Ports = formatPorts(c.Ports)
		out = append(out, ci)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func formatPorts(ports []docker.ContainerPort) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		if p.PublicPort == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d->%d/%s", p.PublicPort, p.PrivatePort, p.Type))
	}
	return strings.Join(parts, ", ")
}

func (a *API) containers(w http.ResponseWriter, r *http.Request) {
	cs, err := a.LocalContainers(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": hostnameOr(""), "containers": cs})
}

// restartReq 容器重启请求（name 为容器名或 ID）。
type restartReq struct {
	Name string `json:"name"`
}

// restartResp 容器重启结果。
type restartResp struct {
	Node      string `json:"node"`
	Container string `json:"container"`
	OK        bool   `json:"ok"`
	Message   string `json:"message,omitempty"`
}

// restartContainer POST /api/v1/local/containers/restart
// 重启本机一个容器（docker restart）。独立部署（docker run）的中间件容器
// （如 r-nacos）在服务视图之外，管理端由此触发重启。
func (a *API) restartContainer(w http.ResponseWriter, r *http.Request) {
	var req restartReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, errEmpty("name"))
		return
	}
	if err := a.cli.RestartContainer(r.Context(), req.Name); err != nil {
		writeJSON(w, http.StatusBadGateway, restartResp{
			Node: hostnameOr(""), Container: req.Name, OK: false, Message: err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, restartResp{Node: hostnameOr(""), Container: req.Name, OK: true})
}

func hostnameOr(fallback string) string {
	if h, err := hostname(); err == nil {
		return h
	}
	return fallback
}
