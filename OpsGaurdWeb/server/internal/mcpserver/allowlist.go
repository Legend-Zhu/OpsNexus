// worker_url CIDR 白名单（P3 可选加固）：mcp.cluster_url_allow_cidrs 非空时，
// cluster_add / cluster_update 只接受目标网段内的 Worker 地址，收敛
// 「任意内网 URL 可被 gRPC 探测」的 SSRF 面。
package mcpserver

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

// checkClusterURLAllowed 校验 worker_url 是否在白名单网段内。
// list 为空 = 不限制（放行）。url 支持 host:port 或完整 URL 形态；
// 域名按其解析到的首个地址校验（内网部署域名一般直解）。
func (h *Handler) checkClusterURLAllowed(raw string) error {
	list := h.deps.Config.ClusterURLAllowCIDRs
	if len(list) == 0 {
		return nil
	}
	host := extractHost(raw)
	if host == "" {
		return fmt.Errorf("worker_url %q has no host", raw)
	}
	// 域名 → IP（解析失败按原样当作地址尝试）
	ips := []net.IP{net.ParseIP(host)}
	if ips[0] == nil {
		resolved, err := net.LookupIP(host)
		if err != nil || len(resolved) == 0 {
			return fmt.Errorf("cannot resolve worker_url host %q for allowlist check", host)
		}
		ips = resolved
	}
	var prefixes []netip.Prefix
	for _, cidr := range list {
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			return fmt.Errorf("mcp.cluster_url_allow_cidrs entry %q invalid: %v", cidr, err)
		}
		prefixes = append(prefixes, p)
	}
	for _, ip := range ips {
		addr, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		addr = addr.Unmap()
		for _, p := range prefixes {
			if p.Contains(addr) {
				return nil
			}
		}
	}
	return fmt.Errorf("worker_url host %q is outside mcp.cluster_url_allow_cidrs %s — ask the admin to widen the allowlist",
		host, strings.Join(list, ","))
}

// extractHost 从 host:port 或 URL 形态提取 host。
func extractHost(raw string) string {
	s := strings.TrimSpace(raw)
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		return host
	}
	return s
}
