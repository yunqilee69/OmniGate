// Package vkctx 提供代理面与管理面共享的虚拟 key context 键。
// Go 的 context key 按 (type, value) 比较；跨包各自定义同名类型会导致取值恒 miss。
package vkctx

import (
	"context"

	"github.com/cloudomni/omnigate/internal/store"
)

type contextKey string

// Key 是注入/读取 *store.VirtualKey 的唯一 context 键。
const Key contextKey = "virtual_key"

// From 从 context 取出虚拟 key。
func From(ctx context.Context) (*store.VirtualKey, bool) {
	vk, ok := ctx.Value(Key).(*store.VirtualKey)
	return vk, ok
}
