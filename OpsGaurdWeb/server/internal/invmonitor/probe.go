package invmonitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy"
)

// checkSpec 一个可执行检查：id 在 (cluster, item) 内唯一，run 返回确定性
// 探测结果；RPC 执行失败（节点不可达等）经 err 返回，不参与状态翻转。
type checkSpec struct {
	id       string
	evType   store.EventType
	interval time.Duration
	run      func(ctx context.Context) (ok bool, msg string, err error)
}

// checkIDs 返回条目全部检查 id（配置视角，与探测执行无关，供状态清理对账）。
func checkIDs(item *store.InventoryItem) []string {
	m := item.Monitoring
	var ids []string
	if item.Type == store.InvStandaloneContainer {
		ids = append(ids, "container")
	}
	for _, pc := range m.PortChecks {
		ids = append(ids, "port:"+pc.Port)
	}
	for _, hc := range m.HTTPChecks {
		ids = append(ids, "http:"+hc.URL)
	}
	return ids
}

// runItem 执行一个纳管条目的到期检查。
func (s *Service) runItem(ctx context.Context, clusterName string, cli *workerproxy.Client, item *store.InventoryItem) {
	specs := s.buildChecks(ctx, cli, item)
	for _, spec := range specs {
		key := clusterName + "/" + item.Name + "/" + spec.id
		if !s.due(key, spec.interval) {
			continue
		}
		ok, msg, rpcErr := spec.run(ctx)
		s.recordResult(clusterName, item.Name, spec, ok, msg, rpcErr)
	}
}

// buildChecks 按条目类型展开检查列表：
//   - host-service: portChecks → CheckPort（目标 host 取 ref，缺省回退 item.Node）；
//     httpChecks → CheckHTTP（URL 按声明，Worker 侧从 item.Node 发起）
//   - standalone-container: 容器存活（NodeContainers 按 ref 匹配，状态非 running
//     或消失即失败）；portChecks → 对节点地址探测声明端口
func (s *Service) buildChecks(ctx context.Context, cli *workerproxy.Client, item *store.InventoryItem) []checkSpec {
	m := item.Monitoring
	var specs []checkSpec

	switch item.Type {
	case store.InvHostService:
		host, _ := parseHostPort(item.Ref)
		if host == "" {
			host = item.Node
		}
		for _, pc := range m.PortChecks {
			pc := pc
			port, err := strconv.Atoi(pc.Port)
			if err != nil {
				continue
			}
			specs = append(specs, checkSpec{
				id:       "port:" + pc.Port,
				evType:   store.EventPortDown,
				interval: parseInterval(pc.Interval, defaultPortInterval),
				run: func(ctx context.Context) (bool, string, error) {
					r, err := cli.CheckPort(ctx, item.Node, host, port, timeoutOr(pc.Timeout, "3s"))
					if err != nil {
						return false, "", err
					}
					if !r.OK {
						return false, fmt.Sprintf("TCP %s:%d 不可达：%s", host, port, r.Error), nil
					}
					return true, "", nil
				},
			})
		}
		for _, hc := range m.HTTPChecks {
			hc := hc
			specs = append(specs, checkSpec{
				id:       "http:" + hc.URL,
				evType:   store.EventHTTPUnhealthy,
				interval: parseInterval(hc.Interval, defaultHTTPInterval),
				run: func(ctx context.Context) (bool, string, error) {
					r, err := cli.CheckHTTP(ctx, item.Node, workerproxy.HTTPCheckRequest{
						URL: hc.URL, Method: hc.Method, Headers: hc.Headers,
						ExpectedStatus: hc.ExpectedStatus, ExpectedBody: hc.ExpectedBody,
						Timeout: timeoutOr(hc.Timeout, "5s"),
					})
					if err != nil {
						return false, "", err
					}
					if !r.OK {
						return false, fmt.Sprintf("HTTP %s 健康检查失败：status=%d %s", hc.URL, r.Status, r.Error), nil
					}
					return true, "", nil
				},
			})
		}

	case store.InvStandaloneContainer:
		specs = append(specs, checkSpec{
			id:       "container",
			evType:   store.EventContainerDown,
			interval: defaultStateInterval,
			run: func(ctx context.Context) (bool, string, error) {
				containers, err := cli.NodeContainers(ctx, item.Node)
				if err != nil {
					return false, "", err
				}
				for _, ct := range containers {
					if strings.TrimPrefix(ct.Name, "/") == item.Ref || ct.Name == item.Ref {
						if ct.State != "running" {
							return false, fmt.Sprintf("容器 %s 状态异常：%s", item.Ref, ct.State), nil
						}
						return true, "", nil
					}
				}
				return false, fmt.Sprintf("容器 %s 在节点 %s 上不存在", item.Ref, item.Node), nil
			},
		})
		if len(m.PortChecks) > 0 {
			// 端口探测目标 = 容器所在节点地址（经节点表解析，失败回退 hostname）。
			addr := s.resolveNodeAddr(ctx, cli, item.Node)
			for _, pc := range m.PortChecks {
				pc := pc
				port, err := strconv.Atoi(pc.Port)
				if err != nil {
					continue
				}
				specs = append(specs, checkSpec{
					id:       "port:" + pc.Port,
					evType:   store.EventPortDown,
					interval: parseInterval(pc.Interval, defaultPortInterval),
					run: func(ctx context.Context) (bool, string, error) {
						r, err := cli.CheckPort(ctx, item.Node, addr, port, timeoutOr(pc.Timeout, "3s"))
						if err != nil {
							return false, "", err
						}
						if !r.OK {
							return false, fmt.Sprintf("TCP %s:%d 不可达：%s", addr, port, r.Error), nil
						}
						return true, "", nil
					},
				})
			}
		}
		// httpChecks 与 host-service 同理：URL 按声明（用户写实际地址），从 item.Node 发起。
		for _, hc := range m.HTTPChecks {
			hc := hc
			specs = append(specs, checkSpec{
				id:       "http:" + hc.URL,
				evType:   store.EventHTTPUnhealthy,
				interval: parseInterval(hc.Interval, defaultHTTPInterval),
				run: func(ctx context.Context) (bool, string, error) {
					r, err := cli.CheckHTTP(ctx, item.Node, workerproxy.HTTPCheckRequest{
						URL: hc.URL, Method: hc.Method, Headers: hc.Headers,
						ExpectedStatus: hc.ExpectedStatus, ExpectedBody: hc.ExpectedBody,
						Timeout: timeoutOr(hc.Timeout, "5s"),
					})
					if err != nil {
						return false, "", err
					}
					if !r.OK {
						return false, fmt.Sprintf("HTTP %s 健康检查失败：status=%d %s", hc.URL, r.Status, r.Error), nil
					}
					return true, "", nil
				},
			})
		}
	}
	return specs
}

// resolveNodeAddr 按 hostname/ID 解析节点地址；解析失败回退原样返回
// （worker 侧 ResolveNodeAddr 也支持按 hostname 发起探测）。
func (s *Service) resolveNodeAddr(ctx context.Context, cli *workerproxy.Client, node string) string {
	nodes, err := cli.ListNodes(ctx)
	if err != nil {
		return node
	}
	for _, n := range nodes {
		if n.Hostname == node || n.ID == node {
			if n.Addr != "" {
				return n.Addr
			}
		}
	}
	return node
}

// timeoutOr 缺省超时。
func timeoutOr(raw, def string) string {
	if raw == "" {
		return def
	}
	return raw
}

// parseHostPort 拆分 "host[:port]"（与 api 包内同名helper一致；冒号结尾
// 视为纯 host）。本包不依赖 api 包，保持小函数本地副本。
func parseHostPort(ref string) (host, port string) {
	idx := strings.LastIndex(ref, ":")
	if idx < 0 {
		return ref, ""
	}
	if idx == len(ref)-1 {
		return ref[:idx], ""
	}
	return ref[:idx], ref[idx+1:]
}
