package proxy

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/cloudomni/omnigate/internal/store"
)

// inboundIdentityHeaders 未选模拟组时允许透传的入站身份头（canonical 键）。
var inboundIdentityHeaders = map[string]bool{
	"User-Agent":                                true,
	"X-App":                                     true,
	"X-Title":                                   true,
	"Http-Referer":                              true,
	"Anthropic-Beta":                            true,
	"Anthropic-Dangerous-Direct-Browser-Access": true,
}

// inboundIdentityPrefix 透传的入站身份头前缀（OpenAI SDK 系列埋点头）。
const inboundIdentityPrefix = "X-Stainless-"

// HeaderProfileFor 返回提供商当前生效模拟组的请求头；
// 未配置组、未选组、组名不存在或 JSON 非法（告警，不中断请求）→ nil。
func HeaderProfileFor(p store.Provider) map[string]string {
	profiles, err := store.ParseHeaderProfiles(p.HeaderProfiles)
	if err != nil {
		slog.Warn("header_profiles 解析失败，忽略客户端模拟", "provider", p.Name, "err", err)
		return nil
	}
	if p.ActiveProfile == "" {
		return nil
	}
	for _, prof := range profiles {
		if prof.Name == p.ActiveProfile {
			return prof.Headers
		}
	}
	return nil
}

// ApplyUpstreamIdentity 应用客户端身份到上游请求（唯一入口；调用点在适配器认证头之后、流式 Accept 之前，
// 保证 Accept 不被组覆盖）：
//  1. 生效模拟组存在 → 按组覆盖（跳过 store.ReservedHeaderKeys 保留头）；
//  2. 否则 r 非 nil → 透传入站白名单身份头（各取第一个值）；
//  3. 否则维持现状（Go 默认头）。
//
// r 可为 nil（模型探测/模型拉取等无入站请求场景，仅生效组生效）。
func ApplyUpstreamIdentity(upReq *http.Request, p store.Provider, r *http.Request) {
	if headers := HeaderProfileFor(p); len(headers) > 0 {
		for k, v := range headers {
			if store.ReservedHeaderKeys[strings.ToLower(k)] {
				continue
			}
			upReq.Header.Set(k, v)
		}
		return
	}
	if r == nil {
		return
	}
	for k, vs := range r.Header {
		ck := http.CanonicalHeaderKey(k)
		if !inboundIdentityHeaders[ck] && !strings.HasPrefix(ck, inboundIdentityPrefix) {
			continue
		}
		if len(vs) > 0 {
			upReq.Header.Set(ck, vs[0])
		}
	}
}
