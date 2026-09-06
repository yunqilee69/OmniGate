package proxy

import (
	"context"

	"github.com/cloudomni/omnigate/internal/store"
)
// contextKey 用于从 context 中获取虚拟 key。
type contextKey string

const vkContextKey contextKey = "virtual_key"

var (
	errVKRouteDenied = store.ErrVKAccessDenied
)

// getVKFromContext 从 context 中获取虚拟 key。
func getVKFromContext(ctx context.Context) (*store.VirtualKey, bool) {
	vk, ok := ctx.Value(vkContextKey).(*store.VirtualKey)
	return vk, ok
}

// checkVKRouteAccess 检查虚拟 key 是否允许访问路由。
func checkVKRouteAccess(db *store.Store, vk *store.VirtualKey, routeID int64) error {
	return db.CheckVKRouteAccess(vk, routeID)
}
