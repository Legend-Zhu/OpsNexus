// exec 透传（P3）：经各集群 Worker MCP 的 exec 工具做容器/宿主机命令执行。
// 三重防护（设计方案 §7.4）：mcp.exec_enabled 开关（默认关）+ token
// scope=exec（独立显式授权，read/write 不可用）+ 全量审计（命令原文落库）。
// 链路：管理端 →（进程内 MCP 客户端）→ 目标集群 Worker /mcp → 节点执行，
// 命令仍受 Worker 侧 commandPolicy 黑白名单约束。
package mcpserver

import (
	"context"
	"fmt"
	"time"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// execToolTimeout 单次 exec 的总超时（Worker 侧另有 30s 命令超时兜底）。
const execToolTimeout = 45 * time.Second

type serviceExecIn struct {
	Cluster string   `json:"cluster" description:"Cluster name (from cluster_list)"`
	Service string   `json:"service" description:"Service name to exec into (the tool targets a running task of it)"`
	Command []string `json:"command" description:"Command and args, e.g. [\"ls\", \"-la\", \"/etc\"]"`
	Slot    int      `json:"slot,omitempty" description:"Task slot to target, 1-based (default: first running task)"`
}

type execOut struct {
	Output string `json:"output"`
	Hint   string `json:"hint,omitempty"`
}

type nodeExecIn struct {
	Cluster string `json:"cluster" description:"Cluster name (from cluster_list)"`
	Command string `json:"command" description:"Host command, e.g. \"df -h\" or \"ss -tlnp | grep 8080\""`
	Node    string `json:"node,omitempty" description:"Target node (hostname or id). Empty = ALL nodes"`
}

// execGuard exec 工具统一守卫：配置开关 + exec scope（叠加在端点鉴权之上）。
func (h *Handler) execGuard(ctx context.Context, tool string) error {
	if !h.deps.Config.ExecEnabled {
		return fmt.Errorf("exec passthrough is disabled (set mcp.exec_enabled=true in config.yaml to enable)")
	}
	id := identityFrom(ctx)
	if !id.canExec() {
		return fmt.Errorf("token %q (scope=%s) cannot use %q: exec requires a dedicated scope=exec token — "+
			"it grants command execution on managed clusters and is only issued to fully trusted consumers", id.Name, id.Scope, tool)
	}
	return nil
}

// auditedExec audited 的 exec 变体：execGuard + 审计（命令原文入摘要）。
func auditedExec[In any, Out any](h *Handler, tool string, argsOf func(In) map[string]string,
	fn func(ctx context.Context, in In) (Out, error),
) func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, Out, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		var zero Out
		if err := h.execGuard(ctx, tool); err != nil {
			return nil, zero, err
		}
		start := time.Now()
		out, err := fn(ctx, in)
		h.auditWrite(ctx, tool, summarizeArgs(argsOf(in)), err, start)
		if err != nil {
			return nil, zero, err
		}
		return nil, out, nil
	}
}

// ensureClusterMCP 保证目标集群的 Worker MCP 已连接（exec 走该通道）。
func (h *Handler) ensureClusterMCP(cluster string) error {
	srv := h.gateway()
	if srv == nil {
		return fmt.Errorf("embedded AiNexus gateway is not enabled — exec passthrough routes through its MCP connections")
	}
	for _, n := range srv.MCPNames() {
		if n == "cluster:"+cluster {
			return nil
		}
	}
	url, token, err := h.deps.Clusters.MCPEndpoint(cluster)
	if err != nil {
		return fmt.Errorf("cluster %q: %w", cluster, err)
	}
	return srv.AddMCPCluster(cluster, url, token)
}

// callWorkerTool 调目标集群 Worker MCP 的一个工具并取回文本。
func (h *Handler) callWorkerTool(ctx context.Context, cluster, tool string, args map[string]any) (string, error) {
	if err := h.ensureClusterMCP(cluster); err != nil {
		return "", err
	}
	srv := h.gateway()
	cctx, cancel := context.WithTimeout(ctx, execToolTimeout)
	defer cancel()
	out, isErr, err := srv.CallClusterTool(cctx, cluster, tool, args)
	if err != nil {
		return "", err
	}
	if isErr {
		return "", fmt.Errorf("worker rejected %s: %s", tool, clip(out, errorMax))
	}
	return out, nil
}

func (h *Handler) registerExecTools(s *mcp.Server) {
	// service_exec（容器内执行）
	mcp.AddTool(s, &mcp.Tool{
		Name:        "service_exec",
		Description: "Run a command inside a running task container of a service (diagnostics: cat config, curl localhost, ss -tlnp …). Read-only intent recommended; subject to the worker's command policy. Requires exec scope and mcp.exec_enabled on the server.",
	}, auditedExec(h, "service_exec",
		func(in serviceExecIn) map[string]string {
			return map[string]string{"cluster": in.Cluster, "service": in.Service, "command": fmt.Sprint(in.Command)}
		},
		func(ctx context.Context, in serviceExecIn) (execOut, error) {
			args := map[string]any{"service": in.Service, "command": in.Command, "confirm": true}
			if in.Slot > 0 {
				args["slot"] = in.Slot
			}
			out, err := h.callWorkerTool(ctx, in.Cluster, "exec_in_container", args)
			if err != nil {
				return execOut{}, err
			}
			return execOut{Output: truncStr(out)}, nil
		}))

	// node_exec（宿主机执行）
	mcp.AddTool(s, &mcp.Tool{
		Name:        "node_exec",
		Description: "Run a command on the host OS of a node via nsenter (network/namespace diagnostics: ss, ip addr, df). node empty = ALL nodes — be explicit. Highest-risk tool: requires exec scope + mcp.exec_enabled; worker command policy still applies.",
	}, auditedExec(h, "node_exec",
		func(in nodeExecIn) map[string]string {
			return map[string]string{"cluster": in.Cluster, "node": in.Node, "command": in.Command}
		},
		func(ctx context.Context, in nodeExecIn) (execOut, error) {
			if in.Node == "" {
				return execOut{}, fmt.Errorf("node is required (empty would fan out to every node; pass an explicit node from node_list)")
			}
			args := map[string]any{"command": in.Command, "node": in.Node, "confirm": true}
			out, err := h.callWorkerTool(ctx, in.Cluster, "exec_host_command", args)
			if err != nil {
				return execOut{}, err
			}
			return execOut{Output: truncStr(out)}, nil
		}))
}
