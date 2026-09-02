package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/cloudomni/omnigate/internal/store"
)

// contextKey 用于存储虚拟 key 到 context。
type contextKey string

const vkContextKey contextKey = "virtual_key"

// VKAuthMiddleware 虚拟 key 鉴权中间件。
// 从 Authorization 头提取 Bearer token，验证并加载虚拟 key，注入到 context。
func VKAuthMiddleware(db *store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if auth == "" {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeErr(w, 401, "missing_auth", "Authorization header required")
				return
			}

			// 提取 Bearer token
			parts := strings.SplitN(auth, " ", 2)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				writeErr(w, 401, "invalid_auth", "Authorization must be Bearer token")
				return
			}
			keyValue := strings.TrimSpace(parts[1])

			// 验证虚拟 key
			vk, err := db.CheckVKAuth(keyValue)
			if err != nil {
				switch err {
				case store.ErrVKNotFound:
					writeErr(w, 401, "invalid_key", "invalid virtual key")
				case store.ErrVKDisabled:
					writeErr(w, 403, "key_disabled", "virtual key is disabled")
				default:
					writeErr(w, 500, "auth_error", err.Error())
				}
				return
			}

			// 将虚拟 key 注入 context
			ctx := context.WithValue(r.Context(), vkContextKey, vk)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// GetVKFromContext 从 context 中获取虚拟 key。
func GetVKFromContext(ctx context.Context) (*store.VirtualKey, bool) {
	vk, ok := ctx.Value(vkContextKey).(*store.VirtualKey)
	return vk, ok
}

// VKRateLimitMiddleware 虚拟 key 限流中间件。
// 检查 RPM/TPM 限制，在请求前记录命中。
func VKRateLimitMiddleware(db *store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			vk, ok := GetVKFromContext(r.Context())
			if !ok {
				// 中间件链断裂，不应该发生
				writeErr(w, 500, "internal_error", "virtual key not found in context")
				return
			}

			// 检查限流
			if err := db.CheckVKRateLimit(vk.ID, vk.RPMLimit, vk.TPMLimit); err != nil {
				if err == store.ErrVKRateLimited {
					w.Header().Set("X-RateLimit-Limit-Requests", formatInt64(vk.RPMLimit))
					w.Header().Set("X-RateLimit-Limit-Tokens", formatInt64(vk.TPMLimit))
					writeErr(w, 429, "rate_limit_exceeded", "rate limit exceeded")
				} else {
					writeErr(w, 500, "rate_limit_error", err.Error())
				}
				return
			}

			// 记录命中（先记录 0 token，实际 token 数在响应后更新）
			if err := db.RecordVKRateLimitHit(vk.ID, 0); err != nil {
				slog.Warn("failed to record rate limit hit", "vk_id", vk.ID, "err", err)
			}

			next.ServeHTTP(w, r)
		})
	}
}

// VKBudgetMiddleware 虚拟 key 配额检查中间件（预检查）。
func VKBudgetMiddleware(db *store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			vk, ok := GetVKFromContext(r.Context())
			if !ok {
				writeErr(w, 500, "internal_error", "virtual key not found in context")
				return
			}

			// 检查配额
			if err := db.CheckVKBudget(vk); err != nil {
				if err == store.ErrVKBudgetExceeded {
					w.Header().Set("X-Budget-Limit", formatFloat64(vk.BudgetUSD))
					w.Header().Set("X-Budget-Used", formatFloat64(vk.UsedUSD))
					writeErr(w, 402, "budget_exceeded", "budget exceeded")
				} else {
					writeErr(w, 500, "budget_error", err.Error())
				}
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// CheckVKModelAccess 虚拟 key 模型访问控制检查（在 proxy handler 内部调用）。
func CheckVKModelAccess(db *store.Store, vk *store.VirtualKey, model string) error {
	return db.CheckVKModelAccess(vk, model)
}

func formatInt64(v int64) string {
	if v == 0 {
		return "unlimited"
	}
	return strconv.FormatInt(v, 10)
}

func formatFloat64(v float64) string {
	if v == 0 {
		return "unlimited"
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}
