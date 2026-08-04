package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是全局配置结构（vendored from ./AiNexus，嵌入管理端）
type Config struct {
	Enabled    bool              `yaml:"enabled"` // 内嵌网关开关（管理端进程内启用）
	Server     ServerConfig      `yaml:"server"`  // 独立部署时的 HTTP 配置；内嵌时忽略
	Providers  []ProviderConfig  `yaml:"providers"`
	Tools      ToolsConfig       `yaml:"tools"`
	MCPServers []MCPServerConfig `yaml:"mcp_servers"`
	Agent      AgentConfig       `yaml:"agent"`
}

// ServerConfig HTTP 服务配置
type ServerConfig struct {
	Addr   string `yaml:"addr"`
	APIKey string `yaml:"api_key"`
}

// ProviderType 提供商 API 格式类型
type ProviderType string

const (
	// ProviderTypeOpenAI 兼容 OpenAI Chat Completion API 格式
	// 适用：OpenAI、GLM、DeepSeek、Moonshot、Ollama、vLLM、LiteLLM 等
	ProviderTypeOpenAI ProviderType = "openai_compatible"

	// ProviderTypeAnthropic 兼容 Anthropic Messages API 格式
	// 适用：Anthropic Claude 等
	ProviderTypeAnthropic ProviderType = "anthropic_compatible"
)

// ProviderConfig 提供商配置（支持多模型）
type ProviderConfig struct {
	Name    string        `yaml:"name"`              // 提供商显示名称，如 "openai"、"zhipu"、"deepseek"
	Type    ProviderType  `yaml:"type"`              // API 格式类型: openai_compatible | anthropic_compatible
	BaseURL string        `yaml:"base_url"`          // API 基础地址
	APIKey  string        `yaml:"api_key"`           // API Key
	Models  []ModelConfig `yaml:"models"`            // 该提供商下的模型列表
	Extra   map[string]any `yaml:"extra,omitempty"`  // 扩展字段（如自定义请求头等）
}

// ModelConfig 模型配置
type ModelConfig struct {
	Name        string  `yaml:"name"`                  // 模型标识（请求时传的 model 值），如 "glm-5.1"、"gpt-5.4"
	DisplayName string  `yaml:"display_name,omitempty"` // 显示名称（可选），如 "GLM 5.1"
	MaxTokens   int     `yaml:"max_tokens,omitempty"`   // 默认最大输出 token
	Temperature float64 `yaml:"temperature,omitempty"`  // 默认温度
}

// ToolsConfig 内置工具配置
type ToolsConfig struct {
	Command     CommandToolConfig     `yaml:"command"`
	HTTPRequest HTTPRequestToolConfig `yaml:"http_request"`
	FileRead    FileReadToolConfig    `yaml:"file_read"`
}

// CommandToolConfig 命令执行工具配置
type CommandToolConfig struct {
	Enabled         bool          `yaml:"enabled"`
	AllowedCommands []string      `yaml:"allowed_commands"`
	Timeout         time.Duration `yaml:"timeout"`
	WorkDir         string        `yaml:"work_dir"`
}

// HTTPRequestToolConfig HTTP 请求工具配置
type HTTPRequestToolConfig struct {
	Enabled bool          `yaml:"enabled"`
	Timeout time.Duration `yaml:"timeout"`
}

// FileReadToolConfig 文件读取工具配置
type FileReadToolConfig struct {
	Enabled bool  `yaml:"enabled"`
	MaxSize int64 `yaml:"max_size"`
}

// MCPServerConfig MCP Server 配置
type MCPServerConfig struct {
	Name      string            `yaml:"name"`
	Transport string            `yaml:"transport"` // stdio, sse, streamable-http
	Command   string            `yaml:"command"`
	Args      []string          `yaml:"args"`
	Env       map[string]string `yaml:"env"`
	URL       string            `yaml:"url"`
	Headers   map[string]string `yaml:"headers"`
}

// AgentConfig Agent 配置
type AgentConfig struct {
	MaxToolRounds     int  `yaml:"max_tool_rounds"`
	ParallelToolCalls bool `yaml:"parallel_tool_calls"`
	// MaxContextTokens 上下文 token 预算（0=不裁剪）：超过后按完整轮次裁剪旧历史。
	MaxContextTokens int `yaml:"max_context_tokens,omitempty"`
	// KeepToolRounds 裁剪时保留最近多少个工具轮次（默认 3）。
	KeepToolRounds int `yaml:"keep_tool_rounds,omitempty"`
}

// DefaultConfig 返回默认配置
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Addr: ":8080",
		},
		Providers: []ProviderConfig{},
		Tools: ToolsConfig{
			Command: CommandToolConfig{
				Enabled: true,
				Timeout: 30 * time.Second,
				WorkDir: "/tmp",
			},
			HTTPRequest: HTTPRequestToolConfig{
				Enabled: true,
				Timeout: 15 * time.Second,
			},
			FileRead: FileReadToolConfig{
				Enabled: true,
				MaxSize: 1048576,
			},
		},
		Agent: AgentConfig{
			// 深度排查/巡检可能执行大量工具调用采集证据；1M 上下文 + 摘要
			// 压缩兜底，工具轮次上限给足 200。
			MaxToolRounds:     200,
			ParallelToolCalls: true,
			// 对齐深度排查模型 1M 窗口（预留工具定义/系统提示余量）。
			// 超预算时优先摘要压缩旧轮次（类 Trae Memory），非硬删。
			MaxContextTokens: 800000,
			KeepToolRounds:   5,
		},
	}
}

// Load 从文件加载配置
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config file: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
}

// Validate 校验配置
func (c *Config) Validate() error {
	// 内嵌模式下 server.addr 无意义（复用管理端监听），不强制
	if !c.Enabled && c.Server.Addr == "" {
		return fmt.Errorf("server.addr is required when ainexus not embedded")
	}
	if len(c.Providers) == 0 {
		return fmt.Errorf("at least one provider must be configured")
	}

	modelNames := make(map[string]string) // model_name -> provider_name，用于检测重复
	for i, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("providers[%d].name is required", i)
		}
		switch p.Type {
		case ProviderTypeOpenAI, ProviderTypeAnthropic:
			// OK
		default:
			return fmt.Errorf("providers[%d].type must be openai_compatible or anthropic_compatible, got %q", i, p.Type)
		}
		if p.BaseURL == "" {
			return fmt.Errorf("providers[%d].base_url is required", i)
		}
		if p.APIKey == "" {
			return fmt.Errorf("providers[%d].api_key is required", i)
		}
		if len(p.Models) == 0 {
			return fmt.Errorf("providers[%d] must have at least one model", i)
		}
		for _, m := range p.Models {
			if m.Name == "" {
				return fmt.Errorf("providers[%d] has a model with empty name", i)
			}
			if existing, ok := modelNames[m.Name]; ok {
				return fmt.Errorf("model name %q is duplicated in providers %q and %q", m.Name, existing, p.Name)
			}
			modelNames[m.Name] = p.Name
		}
	}

	for i, mcp := range c.MCPServers {
		if mcp.Name == "" {
			return fmt.Errorf("mcp_servers[%d].name is required", i)
		}
		switch mcp.Transport {
		case "stdio":
			if mcp.Command == "" {
				return fmt.Errorf("mcp_servers[%d].command is required for stdio transport", i)
			}
		case "sse", "streamable-http":
			if mcp.URL == "" {
				return fmt.Errorf("mcp_servers[%d].url is required for %s transport", i, mcp.Transport)
			}
		default:
			return fmt.Errorf("mcp_servers[%d].transport must be one of: stdio, sse, streamable-http", i)
		}
	}
	return nil
}
