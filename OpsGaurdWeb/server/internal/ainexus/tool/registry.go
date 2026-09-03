package tool

import (
	"fmt"
	"sync"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/provider"
)

// Registry 工具注册中心
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry 创建工具注册中心
func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]Tool),
	}
}

// Register 注册工具
func (r *Registry) Register(t Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	name := t.Name()
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("tool %q already registered", name)
	}
	r.tools[name] = t
	return nil
}

// MustRegister 注册工具，重复则 panic
func (r *Registry) MustRegister(t Tool) {
	if err := r.Register(t); err != nil {
		panic(err)
	}
}

// Get 获取工具
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Unregister 按 tool.Name() 注销工具（MCP server 移除/重建时清理其工具，
// 防止连接已断的"幽灵工具"继续暴露给模型）。返回是否确有删除。
func (r *Registry) Unregister(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[name]; !ok {
		return false
	}
	delete(r.tools, name)
	return true
}

// List 列出所有工具
func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		result = append(result, t)
	}
	return result
}

// ToolDefinitions 获取所有工具的 Provider 定义格式
func (r *Registry) ToolDefinitions() []provider.ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]provider.ToolDefinition, 0, len(r.tools))
	for _, t := range r.tools {
		result = append(result, provider.ToolDefinition{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	return result
}

// ToolCount 已注册工具数量
func (r *Registry) ToolCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}
