package proxy

import (
	"context"

	"github.com/cloudomni/omnigate/internal/store"
	"github.com/cloudomni/omnigate/internal/vkctx"
)

// getVKFromContext 从 context 中获取虚拟 key。
// 统一走 vkctx 共享键：api 中间件注入、proxy 处理器读取必须用同一个 (type, value)。
func getVKFromContext(ctx context.Context) (*store.VirtualKey, bool) {
	return vkctx.From(ctx)
}

// checkVKRouteAccess 检查虚拟 key 是否允许访问路由。
func checkVKRouteAccess(db *store.Store, vk *store.VirtualKey, routeID int64) error {
	return db.CheckVKRouteAccess(vk, routeID)
}
