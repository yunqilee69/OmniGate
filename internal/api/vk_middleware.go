package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/cloudomni/omnigate/internal/store"
	"github.com/cloudomni/omnigate/internal/vkctx"
)

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

			// 将虚拟 key 注入 context（vkctx 共享键，代理面同键读取）
			ctx := vkctx.With(r.Context(), vk)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// GetVKFromContext 从 context 中获取虚拟 key。
func GetVKFromContext(ctx context.Context) (*store.VirtualKey, bool) {
	return vkctx.From(ctx)
}

// VKRateLimitMiddleware 虚拟 key 限流中间件。
// 检查 RPM 限制；命中计数在 handler 返回后记录，避免失败请求消耗配额。
func VKRateLimitMiddleware(db *store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			vk, ok := GetVKFromContext(r.Context())
			if !ok {
				writeErr(w, 500, "internal_error", "virtual key not found in context")
				return
			}

			if err := db.CheckVKRateLimit(vk.ID, vk.RPMLimit); err != nil {
				if err == store.ErrVKRateLimitExceeded {
					w.Header().Set("X-RateLimit-Limit-Requests", formatInt64(vk.RPMLimit))
					writeErr(w, 429, "rate_limit_exceeded", "rate limit exceeded")
				} else {
					writeErr(w, 500, "rate_limit_error", err.Error())
				}
				return
			}

			next.ServeHTTP(w, r)

			if err := db.RecordVKRateLimitHit(vk.ID); err != nil {
				slog.Warn("failed to record rate limit hit", "vk_id", vk.ID, "err", err)
			}
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
					w.Header().Set("X-Budget-Limit", formatFloat64(vk.TotalBudgetUSD))
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
