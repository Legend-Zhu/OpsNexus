// Package mcpserver exposes the management plane's capabilities as an MCP
// (Model Context Protocol) server so external AI assistants (ZCode, Claude
// Desktop, Cursor, …) can drive the platform with natural language: add
// projects, onboard clusters, deploy services, upload zip packages to build
// images, and troubleshoot faults — including delegating deep-dive
// investigations to the embedded AiNexus agent.
//
// It is the mirror image of the Worker MCP server (Worker/internal/mcp):
// same official go-sdk, same stateless Streamable HTTP transport, same
// confirm=true guard convention, same "tools are thin wrappers over the
// service layer" rule — zero business logic here. Auth is separate from the
// user session model: named static tokens with read/write scopes (P1 from
// config.yaml; P2 adds a management UI backed by LevelDB).
package mcpserver

// Config 管理端 MCP Server 配置（config.yaml 的 mcp 段）。
type Config struct {
	// Enabled 打开 /mcp 端点。关闭时端点不注册。
	Enabled bool `yaml:"enabled" json:"enabled"`
	// MaxInvestigations 委托排查（investigation_start）的并发上限；
	// 超限返回排队提示而非静默堆积。默认 4。
	MaxInvestigations int `yaml:"max_investigations" json:"maxInvestigations"`
	// ExecEnabled 打开 exec 透传（service_exec / node_exec，经各集群
	// Worker MCP 的 exec 工具）。默认关闭；开启后仍要求 token scope=exec。
	ExecEnabled bool `yaml:"exec_enabled" json:"execEnabled"`
	// ClusterURLAllowCIDRs cluster_add/cluster_update 的 worker_url 白名单
	//（CIDR 列表；空 = 不限制）。收敛"任意内网 URL 可被探测"的 SSRF 面：
	// 只允许把白名单网段内的 Worker 纳管。示例：["10.0.0.0/8","192.168.0.0/16"]。
	ClusterURLAllowCIDRs []string `yaml:"cluster_url_allow_cidrs" json:"clusterUrlAllowCidrs"`
	// Tokens 命名 MCP token（静态种子）。P1 必填：没有可用 token 时
	// /mcp 对一切请求返回 401（MCP 面向 AI 自主调用，平台认证整体
	// 关闭的内网 bootstrap 模式也不例外）。
	Tokens []TokenConfig `yaml:"tokens" json:"-"`
}

// TokenConfig 单个 MCP token。
type TokenConfig struct {
	// Name token 名：即审计 actor，建议按消费者命名（zcode / patrol-bot）。
	Name string `yaml:"name" json:"name"`
	// Secret bearer 原文（建议 32 字节 hex）。内存中只保留 SHA-256 摘要。
	Secret string `yaml:"secret" json:"-"`
	// Scope read | write | exec；read < write < exec（exec 含 write）。
	// exec 为最高危档（容器/宿主机命令执行），只发给受完全信任的消费者。
	Scope string `yaml:"scope" json:"scope"`
}

// 默认值。
func (c *Config) applyDefaults() {
	if c.MaxInvestigations <= 0 {
		c.MaxInvestigations = 4
	}
}
