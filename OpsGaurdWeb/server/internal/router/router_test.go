package router

import (
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/api"
)

// TestNewNoRouteConflict 构造路由表，确保新增路由不与既有路由冲突
// （gin 注册冲突路由时 panic，编译期发现不了）。
func TestNewNoRouteConflict(t *testing.T) {
	if r := New(api.NewHandlers()); r == nil {
		t.Fatal("nil engine")
	}
}
