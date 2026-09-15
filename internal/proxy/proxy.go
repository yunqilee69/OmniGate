// Package proxy 实现 OpenAI 兼容的转发面：三级选择、SSE 透传、统计落库。
package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudomni/omnigate/internal/breaker"
	"github.com/cloudomni/omnigate/internal/config"
	"github.com/cloudomni/omnigate/internal/router"
	"github.com/cloudomni/omnigate/internal/store"
)

const maxBodyBytes = 32 << 20

// 网关自造错误码：日志 request_log.error_code 与客户端 error.code 同一套可读全称。
// 上游 HTTP 状态码仍记数字字符串（"401"/"429"/"500" 等），不在此列。
const (
	errConnectionFailed       = "connection_failed"
	errTimeout                = "timeout"
	errAllBackendsUnavailable = "all_backends_unavailable"
	errAllRetriesFailed       = "all_retries_failed"
	errReadFailed             = "read_failed"
	errStreamSetupFailed      = "stream_setup_failed"
	errEmptyStream            = "empty_stream"
	errStreamBroken           = "stream_broken"
	errClientDisconnected     = "client_disconnected"
	errProtocolConvertFailed  = "protocol_convert_failed"
	errResponseConvertFailed  = "response_convert_failed"
	errMarshalFailed          = "marshal_failed"
	errBadUpstreamURL         = "bad_upstream_url"
	errAudioTooLarge          = "audio_too_large"
)

type Handler struct {
	db          *store.Store
	rt          *config.RuntimeManager
	sel         *router.Selector
	rec         *breaker.Recorder
	client      *http.Client
	clientCache sync.Map
}

func New(db *store.Store, rt *config.RuntimeManager) *Handler {
	return &Handler{
		db: db, rt: rt, sel: router.NewSelector(db), rec: breaker.New(db),
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// HTTPClientFor 按提供商 ProxyURL 构造一次性 HTTP 客户端。
// 空或非法 ProxyURL 直连。timeout<=0 时 Client.Timeout 为零值，即无客户端级超时。
func HTTPClientFor(p store.Provider, timeout time.Duration) *http.Client {
	c := &http.Client{Timeout: timeout}
	if p.ProxyURL == "" {
		return c
	}
	proxyURL, err := url.Parse(p.ProxyURL)
	if err != nil {
		return c
	}
	c.Transport = &http.Transport{
		Proxy:               http.ProxyURL(proxyURL),
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}
	return c
}

const defaultProviderTimeout = 120 * time.Second

// providerTimeout 把提供商 TimeoutMs 转成 AfterFunc 间隔。
// <=0 视为未配置，回退 120s：AfterFunc(0) 会立刻 fire，把请求当成超时。
func providerTimeout(ms int) time.Duration {
	if ms <= 0 {
		return defaultProviderTimeout
	}
	return time.Duration(ms) * time.Millisecond
}

// clientForProvider 返回按提供商配置了代理的 HTTP 客户端。
// ProxyURL 支持以下格式：
//   - http://host:port
//   - http://user:pass@host:port
//   - socks5://host:port
//   - socks5://user:pass@host:port
//
// 空 ProxyURL 返回默认客户端（直连）。
func (h *Handler) clientForProvider(providerID int64) *http.Client {
	if providerID == 0 {
		return h.client
	}
	if cached, ok := h.clientCache.Load(providerID); ok {
		return cached.(*http.Client)
	}

	var p store.Provider
	if err := h.db.DB.First(&p, providerID).Error; err != nil {
		return h.client
	}

	client := h.client
	if p.ProxyURL != "" {
		client = HTTPClientFor(p, 0)
	}

	actual, _ := h.clientCache.LoadOrStore(providerID, client)
	return actual.(*http.Client)
}

// invalidateProviderCache 清除指定提供商的客户端缓存（配置变更时调用）。
func (h *Handler) InvalidateProviderCache(providerID int64) {
	h.clientCache.Delete(providerID)
}

type usageInfo struct {
	prompt     int
	completion int
	cached     int // cache 命中（OpenAI cached ⊆ prompt；Anthropic cache_read 独立）
	cacheWrite int // Anthropic cache_creation_input_tokens；OpenAI 无此字段
	estimated  bool
	seconds    float64 // STT 音频时长（秒）
	chars      int     // TTS 输入字符数
}

type attemptResult struct {
	att router.Attempt
	// promptChars 是请求侧文本量，上游缺 usage 时用于估算 prompt token
	promptChars int
	committed   bool
	retryable   bool
	errCode     string
	status      string
	httpStatus  int
	usage       usageInfo
	ttft        time.Duration
	latencyMs   int64
	elapsed     time.Duration // attempt 开始到结束的总耗时（与 latencyMs 同时刻采集，供 TPS 计算）
	retryAfterS int
	streamBroke bool
	errorBody   string      // 上游错误响应体摘要（< 2KB）；仅错误路径填充
	respHeaders http.Header // 上游响应头快照；内容捕获开启时随 content_log 落库
	reqURL      string      // 出站请求完整 URL（含 path 与 query，query 密钥参数已脱敏）；内容捕获开启时随 content_log 落库
	reqHeaders  string      // 出站请求头快照（OmniGate → 上游，格式化+脱敏）；内容捕获开启时随 content_log 落库
	reqBody     []byte      // 出站请求体（协议转换/model 替换后实际发送的字节）；内容捕获开启时随 content_log 落库
}

func openAIError(w http.ResponseWriter, status int, code, msg string, detail any) {
	errObj := map[string]any{
		"message": msg,
		"type":    "invalid_request_error",
		"code":    code,
	}
	if detail != nil {
		errObj["detail"] = detail
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": errObj})
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// createPendingLog 在请求进入后立即创建 pending 状态的日志行，返回其 ID。
// 后续 writeLog 通过该 ID 做 UPDATE 补充最终字段。
// 直达 provider/model 请求在 pending 阶段就写入物理提供商/模型，避免飞行中日志看起来像无名路由。
func (h *Handler) createPendingLog(requestID, routeName, endpoint string, isStream bool, vkID int64, snap *router.Snapshot) int64 {
	entry := store.RequestLog{
		RequestID: requestID,
		Route:     routeName,
		Endpoint:  endpoint,
		Status:    "pending",
		IsStream:  isStream,
		VKID:      vkID,
	}
	if p, m, ok := snapshotDirectTarget(snap); ok {
		entry.Provider = p
		entry.Model = m
	}
	if err := h.db.DB.Create(&entry).Error; err != nil {
		slog.Error("create pending request_log failed", "err", err, "request_id", requestID)
		return 0
	}
	return entry.ID
}

// snapshotDirectTarget 直达快照只有一个物理目标且 Route.ID=0。
// 逻辑路由仍可能有多目标，pending / all_backends 不提前填 provider/model。
func snapshotDirectTarget(snap *router.Snapshot) (provider, model string, ok bool) {
	if snap == nil || snap.Route.ID != 0 || len(snap.Targets) != 1 {
		return "", "", false
	}
	m, mok := snap.Models[snap.Targets[0].ModelID]
	if !mok {
		return "", "", false
	}
	p, pok := snap.Providers[m.ProviderID]
	if !pok {
		return "", "", false
	}
	return p.Name, m.Name, true
}

// captureEnabled 内容捕获开关：全局开启且（白名单空=全捕获，或命中路由名 / 直达 provider/model）。
// 直达请求 routeName 是 "SeekAI/deepseek-..."，白名单勾选逻辑路由或物理模型名都应命中。
func captureEnabled(rt *config.Runtime, routeName string, snap *router.Snapshot) bool {
	if rt == nil || !rt.CaptureEnabled {
		return false
	}
	if len(rt.CaptureRoutes) == 0 {
		return true
	}
	if containsStr(rt.CaptureRoutes, routeName) {
		return true
	}
	if p, m, ok := snapshotDirectTarget(snap); ok {
		return containsStr(rt.CaptureRoutes, p+"/"+m) || containsStr(rt.CaptureRoutes, m)
	}
	return false
}

// emptyAttemptFor 在没有实际转发时构造 Attempt：直达路径填快照里的物理提供商/模型，便于 all_backends 日志可筛。
func emptyAttemptFor(snap *router.Snapshot) router.Attempt {
	p, m, ok := snapshotDirectTarget(snap)
	if !ok {
		return router.Attempt{}
	}
	return router.Attempt{
		Provider: store.Provider{Name: p},
		Model:    store.Model{Name: m},
	}
}

func requestTextChars(req map[string]any) int {
	msgs, ok := req["messages"].([]any)
	if !ok {
		return 0
	}
	total := 0
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok {
			if c, ok := mm["content"].(string); ok {
				total += len([]rune(c))
			}
		}
	}
	return total
}

// nativeTextChars 从原生协议请求体中估算文本字符数（Anthropic/Responses 格式）。
// 用于 fallback token 估算，精度低于真实 usage 但有总比无好。
func nativeTextChars(body []byte) int {
	var req map[string]any
	if json.Unmarshal(body, &req) != nil {
		return len(body) / 3 // JSON 结构开销大，比例调低
	}
	// Anthropic: messages[].content (string 或 [{type:"text",text:"..."}])
	if msgs, ok := req["messages"].([]any); ok {
		return requestTextChars(map[string]any{"messages": msgs})
	}
	// Responses: input[] (与 messages 结构相似)
	if input, ok := req["input"].([]any); ok {
		return requestTextChars(map[string]any{"messages": input})
	}
	return len(body) / 3
}

func estimateUsage(promptChars int, respText string) usageInfo {
	return usageInfo{
		prompt:     promptChars / 4,
		completion: len([]rune(respText)) / 4,
		estimated:  true,
	}
}

// sessionKey 解析会话键：优先自定义请求头；缺失时按消息前缀哈希自动识别。
// 两者皆无返回空串，调用方退化为普通加权随机。
func sessionKey(r *http.Request, req map[string]any, headers []string) string {
	for _, h := range headers {
		if h != "" {
			if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
				return "h:" + v
			}
		}
	}
	return messagePrefixKey(req)
}

// messagePrefixKey 哈希 messages 中第一条 assistant 消息之前的全部消息（role + content 规范化拼接）。
// 该前缀在首轮请求即完整存在、后续每轮字节不变，是跨轮稳定且可从首轮算出的最大切面；
// 首轮有多少条就吃多少条（长 system、few-shot 示例全部进入指纹）。空前缀返回空串。
func messagePrefixKey(req map[string]any) string {
	msgs, _ := req["messages"].([]any)
	var b strings.Builder
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			break
		}
		role, _ := mm["role"].(string)
		if role == "assistant" {
			break
		}
		content, err := json.Marshal(mm["content"])
		if err != nil {
			content = []byte{'?'}
		}
		b.WriteString(role)
		b.WriteByte(0)
		b.Write(content)
		b.WriteByte(0)
	}
	if b.Len() == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "m:" + hex.EncodeToString(sum[:16])
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	requestID := newRequestID()
	w.Header().Set("X-Request-Id", requestID)

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		openAIError(w, 400, errReadFailed, "failed to read request body", nil)
		return
	}
	if len(body) > maxBodyBytes {
		openAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 32MB", nil)
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		openAIError(w, 400, "invalid_json", "request body is not valid JSON", nil)
		return
	}
	routeName, _ := req["model"].(string)
	if routeName == "" {
		openAIError(w, 400, "missing_model", "request body must contain a model field", nil)
		return
	}
	isStream, _ := req["stream"].(bool)

	// 检查虚拟 key 路由访问权限（在 LoadSnapshot 之后，因为需要路由 ID）
	snap, found, err := h.sel.LoadSnapshot(routeName)
	if err != nil {
		slog.Error("load snapshot failed", "err", err, "route", routeName)
		openAIError(w, 500, "internal_error", "failed to load routing config", nil)
		return
	}
	if !found {
		openAIError(w, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("the model '%s' does not exist", routeName), nil)
		return
	}

	var vkID int64
	if vk, ok := getVKFromContext(r.Context()); ok {
		vkID = vk.ID
		if err := checkVKRouteAccess(h.db, vk, snap.Route.ID); err != nil {
			if err == store.ErrVKAccessDenied {
				openAIError(w, 403, "route_denied", fmt.Sprintf("route '%s' not allowed by virtual key", routeName), nil)
			} else {
				openAIError(w, 500, "route_check_error", err.Error(), nil)
			}
			return
		}
	}

	rt := h.rt.Snapshot()
	captureOn := rt.CaptureEnabled && (len(rt.CaptureRoutes) == 0 || containsStr(rt.CaptureRoutes, routeName))
	var cw *captureWriter
	if captureOn {
		cw = newCaptureWriter(w, 1<<20)
		w = cw
		cw.setClientReq(r.Header, body)
	}

	var affKey string
	var affModel int64
	if rt.AffinityEnabled {
		if sk := sessionKey(r, req, rt.AffinityHeaders); sk != "" {
			affKey = routeName + "\x00" + sk
			affModel, _ = h.sel.Affinity(affKey, time.Now())
		}
	}

	pendingID := h.createPendingLog(requestID, routeName, "completions", isStream, vkID)

	tried := map[router.Combo]bool{}
	maxAttempts := rt.BreakerMaxHops + 1
	var last attemptResult
	var errCodes []string
	priorFails := 0
	var attempts []store.RequestAttempt
	finalLogged := false

	useFallback := func() bool {
		fbID := snap.Route.FallbackModelID
		if fbID <= 0 {
			return false
		}
		fallbackAtt, fallbackOK := h.sel.PickFallback(fbID, time.Now())
		if !fallbackOK {
			slog.Warn("fallback model unavailable", "route", routeName, "fallback_model_id", fbID)
			return false
		}
		slog.Info("using fallback model", "route", routeName, "fallback_model_id", fbID)
		attemptStart := time.Now()
		res := h.attempt(w, r, req, fallbackAtt, isStream, rt)
		res.latencyMs, res.elapsed = time.Since(attemptStart).Milliseconds(), time.Since(attemptStart)
		h.record(res, rt)
		attempts = append(attempts, h.attemptRow(requestID, routeName, len(attempts), fallbackAtt, res, attemptStart))
		h.writeLog(start, requestID, routeName, fallbackAtt, isStream,
			res.status, res.errCode, res.usage, res.ttft, time.Since(start), priorFails, res.errorBody, true, vkID, pendingID, attempts)
		cw.setAttempt(res)
		h.maybeCapture(requestID, routeName, cw)
		return true
	}

	for attempt := range maxAttempts {
		att, ok := h.sel.Pick(snap, tried, time.Now(), affModel)
		if !ok {
			if useFallback() {
				return
			}
			if attempt == 0 {
				attempts = append(attempts, h.attemptRow(requestID, routeName, 0, router.Attempt{}, attemptResult{
					status:  "error",
					errCode: errAllBackendsUnavailable,
				}, start))
				statuses := h.sel.BackendStatuses(snap, time.Now())
				h.writeLog(start, requestID, routeName, router.Attempt{}, isStream,
					"error", errAllBackendsUnavailable, usageInfo{}, 0, time.Since(start), priorFails, "", false, vkID, pendingID, attempts)
				openAIError(w, http.StatusServiceUnavailable, errAllBackendsUnavailable,
					fmt.Sprintf("route '%s' has no available backends", routeName), statuses)
				h.maybeCapture(requestID, routeName, cw)
				return
			}
			break
		}
		tried[att.Combo()] = true
		attemptStart := time.Now()
		res := h.attempt(w, r, req, att, isStream, rt)
		res.latencyMs, res.elapsed = time.Since(attemptStart).Milliseconds(), time.Since(attemptStart)
		h.record(res, rt)
		attempts = append(attempts, h.attemptRow(requestID, routeName, attempt, att, res, attemptStart))
		last = res

		if res.committed || !res.retryable {
			h.writeLog(start, requestID, routeName, att, isStream,
				res.status, res.errCode, res.usage, res.ttft, time.Since(start), priorFails, res.errorBody, false, vkID, pendingID, attempts)
			finalLogged = true
			break
		}
		priorFails++
		errCodes = append(errCodes, res.errCode)
		slog.Warn("attempt failed, transferring", "route", routeName,
			"model", att.Model.Name, "key_id", att.Key.ID, "code", res.errCode)
	}

	if !finalLogged {
		if useFallback() {
			return
		}
		if last.att.Model.ID != 0 {
			h.writeLog(start, requestID, routeName, last.att, isStream,
				last.status, last.errCode, last.usage, last.ttft, time.Since(start), priorFails-1, last.errorBody, false, vkID, pendingID, attempts)
		}
	}

	// 所有重试都失败且可重试（没有提交响应），返回 502 Bad Gateway
	if last.att.Model.ID != 0 && !last.committed && last.retryable {
		openAIError(w, http.StatusBadGateway, errAllRetriesFailed,
			fmt.Sprintf("route '%s': all backend attempts failed", routeName), nil)
	}

	// 亲和只在最终成功后回写：失败转移到别的模型成功时，记住的是缓存真正生效的落点。
	if affKey != "" && last.att.Model.ID != 0 && last.status == "success" {
		h.sel.SetAffinity(affKey, last.att.Model.ID, rt.AffinityTTL, time.Now())
	}

	if len(errCodes) > 1 {
		slog.Info("all attempts exhausted", "route", routeName,
			"attempts", len(errCodes), "errors", errCodes)
	}
	if last.att.Model.ID != 0 {
		cw.setAttempt(last)
	}
	h.maybeCapture(requestID, routeName, cw)
}

func (h *Handler) attempt(w http.ResponseWriter, r *http.Request, req map[string]any,
	att router.Attempt, isStream bool, rt *config.Runtime) attemptResult {

	attemptStart := time.Now()
	res := attemptResult{att: att, promptChars: requestTextChars(req)}
	adapter := AdapterFor(att.Model.Protocol)

	// deadline 只约束「建连 + 首字节」：流式首字节到达即停表（stopDeadline），
	// 之后流的生命周期交给 idle reader 的空闲超时，长输出流不会被整体截断；
	// 非流式的响应完成等价于首字节，不停表即覆盖整个响应。
	// TimeoutMs<=0 视为未配置，回退 120s；AfterFunc(0) 会立刻 cancel。
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var timedOut atomic.Bool
	deadline := time.AfterFunc(providerTimeout(att.Provider.TimeoutMs), func() {
		timedOut.Store(true)
		cancel()
	})
	defer deadline.Stop() // attempt 返回后不再需要定时器（流式首字节处已提前停表）
	stopDeadline := func() { deadline.Stop() }

	req["model"] = att.Model.Name
	converted, err := adapter.buildBody(req)
	if err != nil {
		res.errCode, res.status = errProtocolConvertFailed, "error"
		return res
	}
	// 应用 body_override：合并覆盖字段到转换后的请求体
	if att.Model.BodyOverride != "" {
		var override map[string]any
		if err := json.Unmarshal([]byte(att.Model.BodyOverride), &override); err == nil {
			for k, v := range override {
				converted[k] = v
			}
		}
	}
	if isStream && (att.Model.Protocol == "completions" || att.Model.Protocol == "") && rt.StreamInjectUsage {
		so, _ := converted["stream_options"].(map[string]any)
		if so == nil {
			so = map[string]any{}
		}
		so["include_usage"] = true
		converted["stream_options"] = so
	}
	outBody, err := json.Marshal(converted)
	if err != nil {
		res.errCode, res.status = errMarshalFailed, "error"
		return res
	}

	// Debug: 记录即将发送的请求
	if rt.DebugStreamLog {
		slog.Info("[DEBUG] Outbound request to upstream",
			"provider", att.Provider.Name,
			"model", att.Model.Name,
			"key_id", att.Key.ID,
			"protocol", att.Model.Protocol,
			"endpoint", adapter.endpoint(att.Provider.BaseURL, &att.Model),
			"timeout_ms", att.Provider.TimeoutMs,
			"is_stream", isStream,
			"request_body", string(outBody))
	}

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, adapter.endpoint(att.Provider.BaseURL, &att.Model), bytes.NewReader(outBody))
	if err != nil {
		res.errCode, res.status = errBadUpstreamURL, "error"
		return res
	}
	upReq.Header.Set("Content-Type", "application/json")
	extra := map[string]string{}
	adapter.setHeaders(extra, att.Key.KeyValue)
	for k, v := range extra {
		upReq.Header.Set(k, v)
	}
	ApplyUpstreamIdentity(upReq, att.Provider, r)
	if isStream {
		upReq.Header.Set("Accept", "text/event-stream")
	}

	// 出站快照：内容捕获（content_log）记录的是 OmniGate → 上游的实际请求（头已含模拟/认证头）
	res.reqURL = captureURL(upReq.URL)
	res.reqHeaders = formatHeaders(upReq.Header)
	res.reqBody = outBody

	// Debug: 记录请求头（脱敏）
	if rt.DebugStreamLog {
		headers := make(map[string]string)
		for k := range upReq.Header {
			v := upReq.Header.Get(k)
			// 脱敏敏感头
			kl := strings.ToLower(k)
			if strings.Contains(kl, "auth") || strings.Contains(kl, "key") {
				if len(v) > 10 {
					v = v[:10] + "..."
				}
			}
			headers[k] = v
		}
		slog.Info("[DEBUG] Request headers", "headers", headers)
	}

	resp, err := h.clientForProvider(att.Provider.ID).Do(upReq)
	if err != nil {
		if rt.DebugStreamLog {
			slog.Info("[DEBUG] Request failed",
				"error", err.Error(),
				"timed_out", timedOut.Load(),
				"model", att.Model.Name)
		}
		res.retryable, res.status = true, "error"
		if timedOut.Load() || errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
			res.errCode = errTimeout
		} else {
			res.errCode = errConnectionFailed
		}
		return res
	}
	res.respHeaders = resp.Header
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	// Debug: 记录响应状态和头
	if rt.DebugStreamLog {
		headers := make(map[string]string)
		for k := range resp.Header {
			headers[k] = resp.Header.Get(k)
		}
		slog.Info("[DEBUG] Response received",
			"status_code", resp.StatusCode,
			"status", resp.Status,
			"headers", headers,
			"model", att.Model.Name,
			"provider", att.Provider.Name)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		res.httpStatus = resp.StatusCode
		if isStream {
			return h.streamResponse(w, resp, att, attemptStart, cancel, rt, adapter, res, stopDeadline, &timedOut, r.Context())
		}
		return h.bufferedResponse(w, resp, attemptStart, adapter, res, &timedOut)
	}

	res.httpStatus = resp.StatusCode
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	res.errCode = strconv.Itoa(resp.StatusCode)
	res.errorBody = captureErrBody(errBody)

	// Debug: 记录错误响应体
	if rt.DebugStreamLog {
		slog.Info("[DEBUG] Error response body",
			"status_code", resp.StatusCode,
			"body", string(errBody),
			"model", att.Model.Name)
	}

	if resp.StatusCode == 429 {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, convErr := strconv.Atoi(ra); convErr == nil && secs > 0 {
				res.retryAfterS = secs
			}
		}
	}
	retryable := false
	for _, code := range rt.RetryableStatuses {
		if resp.StatusCode == code {
			retryable = true
			break
		}
	}
	if retryable {
		res.retryable, res.status = true, "error"
		return res
	}
	res.committed, res.status = true, "client_error"
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json; charset=utf-8"
	}
	res.errorBody = captureErrBody(errBody)
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(errBody)
	return res
}

func (h *Handler) bufferedResponse(w http.ResponseWriter, resp *http.Response,
	attemptStart time.Time, adapter ProtocolAdapter, res attemptResult, timedOut *atomic.Bool) attemptResult {

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		if timedOut.Load() {
			res.errCode, res.status, res.retryable = errTimeout, "error", true
		} else {
			res.errCode, res.status, res.retryable = errReadFailed, "error", true
		}
		return res
	}

	// Debug: 记录非流式响应体
	if h.rt.Snapshot().DebugStreamLog {
		bodyPreview := string(body)
		if len(bodyPreview) > 2000 {
			bodyPreview = bodyPreview[:2000] + "... (truncated)"
		}
		slog.Info("[DEBUG] Buffered response body received",
			"model", res.att.Model.Name,
			"provider", res.att.Provider.Name,
			"body_size", len(body),
			"body", bodyPreview)
	}

	out, u, convErr := adapter.convertBuffered(body)
	if convErr != nil {
		res.errCode, res.status, res.retryable = errResponseConvertFailed, "error", true
		return res
	}
	if u.prompt > 0 || u.completion > 0 {
		res.usage = u
	} else {
		var parsed struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens        int `json:"prompt_tokens"`
				CompletionTokens    int `json:"completion_tokens"`
				PromptTokensDetails *struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(out, &parsed)
		if parsed.Usage != nil {
			cached := 0
			if parsed.Usage.PromptTokensDetails != nil {
				cached = parsed.Usage.PromptTokensDetails.CachedTokens
			}
			res.usage = usageInfo{prompt: parsed.Usage.PromptTokens, completion: parsed.Usage.CompletionTokens, cached: cached}
		} else {
			respText := ""
			if len(parsed.Choices) > 0 {
				respText = parsed.Choices[0].Message.Content
			}
			res.usage = estimateUsage(res.promptChars, respText)
		}
	}
	res.committed, res.status, res.ttft = true, "success", time.Since(attemptStart)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Modelrouter-Model", res.att.Model.Name)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
	return res
}

func (h *Handler) streamResponse(w http.ResponseWriter, resp *http.Response, att router.Attempt,
	attemptStart time.Time, cancel context.CancelFunc, rt *config.Runtime,
	adapter ProtocolAdapter, res attemptResult, stopDeadline func(), timedOut *atomic.Bool, clientCtx context.Context) attemptResult {

	idle := newIdleReader(resp.Body, time.Duration(rt.StreamIdleTimeoutS)*time.Second, cancel)
	defer idle.Close()
	passthrough := att.Model.Protocol == "completions" || att.Model.Protocol == ""
	scan := newSSEScan()
	splitter := &sseSplitter{}
	var textAcc strings.Builder
	buf := make([]byte, 32<<10)
	flusher, _ := w.(http.Flusher)
	committed := false

	writeToClient := func(lines []string) bool {
		for _, ln := range lines {
			if _, wErr := w.Write([]byte(ln + "\n\n")); wErr != nil {
				res.status, res.errCode = "error", errClientDisconnected
				return false
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	for {
		n, readErr := idle.Read(buf)
		if n > 0 {
			// Debug模式：打印接收到的原始chunk
			if h.rt.Snapshot().DebugStreamLog {
				chunkPreview := string(buf[:n])
				if len(chunkPreview) > 500 {
					chunkPreview = chunkPreview[:500] + "... (truncated)"
				}
				slog.Info("[DEBUG] Stream chunk received",
					"model", att.Model.Name,
					"provider", att.Provider.Name,
					"chunk_size", n,
					"chunk_data", chunkPreview)
			}
			if !committed {
				committed = true
				stopDeadline() // 首字节已到：解除建连+首字节的 deadline，之后流交给 idle 超时
				res.committed = true
				res.ttft = time.Since(attemptStart)
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("X-Modelrouter-Model", att.Model.Name)
				w.WriteHeader(http.StatusOK)
			}
			if passthrough {
				scan.Write(buf[:n])
				if _, wErr := w.Write(buf[:n]); wErr != nil {
					res.status, res.errCode = "error", errClientDisconnected
					return res
				}
				if flusher != nil {
					flusher.Flush()
				}
			} else {
				for _, payload := range splitter.write(buf[:n]) {
					if !writeToClient(adapter.convertStreamChunk(payload)) {
						return res
					}
				}
			}
		}
		if readErr != nil {
			if !passthrough {
				for _, payload := range splitter.flush() {
					if !writeToClient(adapter.convertStreamChunk(payload)) {
						return res
					}
				}
			} else {
				scan.Finish()
			}
			if !committed {
				if errors.Is(readErr, io.EOF) {
					res.status, res.errCode, res.retryable = "error", errEmptyStream, true
				} else if timedOut.Load() {
					res.status, res.errCode, res.retryable = "error", errTimeout, true
				} else {
					res.status, res.errCode, res.retryable = "error", errStreamSetupFailed, true
				}
				return res
			}

			if u := adapter.streamUsage(); u != nil {
				res.usage = *u
			} else if passthrough && scan.Usage() != nil {
				cached := 0
				if scan.Usage().PromptTokensDetails != nil {
					cached = scan.Usage().PromptTokensDetails.CachedTokens
				}
				res.usage = usageInfo{prompt: scan.Usage().PromptTokens, completion: scan.Usage().CompletionTokens, cached: cached}
			} else {
				if passthrough {
					textAcc.WriteString(scan.Text())
				}
				res.usage = estimateUsage(res.promptChars, textAcc.String())
			}

			// 流完整信号：协议结束事件，或 OpenAI 直通的 [DONE]/最终 usage。
			// 仅有 token 不够——Anthropic message_start 首包就带 input_tokens。
			complete := adapter.streamComplete() || (passthrough && (scan.Done() || scan.Usage() != nil))
			if errors.Is(readErr, io.EOF) {
				if !passthrough && complete {
					if !writeToClient(adapter.streamFinal()) {
						return res
					}
				}
				if complete {
					res.status = "success"
				} else {
					res.status, res.errCode, res.streamBroke = "error", errStreamBroken, true
				}
				return res
			}
			if clientCtx != nil && clientCtx.Err() != nil {
				if complete {
					res.status = "success"
				} else {
					res.status, res.errCode = "error", errClientDisconnected
				}
				return res
			}
			if complete {
				res.status = "success"
				return res
			}
			res.status, res.errCode, res.streamBroke = "error", errStreamBroken, true
			if h.rt.Snapshot().DebugStreamLog {
				slog.Info("[DEBUG] Stream broken",
					"model", att.Model.Name,
					"provider", att.Provider.Name,
					"error", readErr.Error(),
					"committed", committed)
			}
			return res
		}
	}
}

// writeLog 落库最终结果：尝试明细、日志终态、日聚合与 VK 用量结算合并为一次事务
// 提交（见 store.SettleRequest）。attempts 为本次请求各跳的尝试明细，按发生顺序落库。
func (h *Handler) writeLog(start time.Time, requestID, routeName string, att router.Attempt,
	isStream bool, status, errCode string, u usageInfo, ttft, total time.Duration,
	retries int, errorBody string, isFallback bool, vkID int64, pendingID int64,
	attempts []store.RequestAttempt) {

	// TotalMs 记端到端墙钟；TPS 必须用最终成功那一跳的生成窗口，
	// 否则前置失败/转移会把 (e2e - lastTTFT) 拉成接近 0 的 tok/s。
	tpsTTFT, tpsTotal := ttft, total
	if n := len(attempts); n > 0 {
		last := attempts[n-1]
		if last.LatencyMs > 0 {
			tpsTotal = time.Duration(last.LatencyMs) * time.Millisecond
			tpsTTFT = time.Duration(last.TTFTMs) * time.Millisecond
		}
	}

	entry := store.RequestLog{
		RequestID: requestID, Route: routeName,
		Status: status, ErrorCode: errCode, ErrorBody: errorBody, IsStream: isStream,
		IsFallback:   isFallback,
		PromptTokens: u.prompt, CompletionTokens: u.completion, CachedTokens: u.cached, TokensEstimated: u.estimated,
		AudioSeconds: u.seconds, InputChars: u.chars,
		TTFTMs: ttft.Milliseconds(), TotalMs: total.Milliseconds(),
		Tps:  streamTPS(isStream, status, u, tpsTTFT, tpsTotal),
		Cost: cost(att.Model, u, h.rt.Snapshot().USDCNY, status), Retries: retries,
		VKID: vkID,
	}
	if att.Model.ID != 0 {
		entry.Model = att.Model.Name
		entry.Provider = att.Provider.Name
		entry.KeyID = att.Key.ID
	}
	if pendingID > 0 {
		entry.ID = pendingID
		// Save 为全列 UPDATE，autoCreateTime 只在 Create 生效：
		// 显式带请求开始时间，避免 pending 行的 created_at 被零值覆盖。
		entry.CreatedAt = start.Unix()
	}
	if err := h.db.SettleRequest(&entry, attempts); err != nil {
		slog.Error("settle request log failed", "err", err, "request_id", requestID)
	}
}

// streamTPS 计算流式输出速度（tok/s）：completion_tokens / 生成窗口（total - ttft）。
// 仅流式成功、usage 非估算且生成时长 >= 500ms 时有意义：非流式（ttft==total，窗口为 0）、
// token 估算、以及极短生成都会产生误导值，一律记 0，不进入统计。
func streamTPS(isStream bool, status string, u usageInfo, ttft, total time.Duration) float64 {
	if !isStream || status != "success" || u.estimated || u.completion <= 0 {
		return 0
	}
	genMs := (total - ttft).Milliseconds()
	if genMs < 500 {
		return 0
	}
	return float64(u.completion) * 1000 / float64(genMs)
}

// attemptRow 构造一次转发尝试的完整明细行（含成功与失败），由调用方累积后随
// writeLog 统一落库，便于排查重试链路。
// 对于 all_backends_unavailable 错误（没有可用模型），model 和 provider 字段为空字符串。
func (h *Handler) attemptRow(requestID, routeName string, attempt int, att router.Attempt,
	res attemptResult, start time.Time) store.RequestAttempt {
	elapsed := res.elapsed
	if elapsed == 0 {
		elapsed = time.Since(start)
	}
	return store.RequestAttempt{
		RequestID:        requestID,
		Route:            routeName,
		Attempt:          attempt,
		Model:            att.Model.Name,    // 空字符串 for all_backends
		Provider:         att.Provider.Name, // 空字符串 for all_backends
		KeyID:            att.Key.ID,        // 0 for all_backends
		Status:           res.status,
		HTTPStatus:       res.httpStatus,
		ErrorCode:        res.errCode,
		ErrorBody:        res.errorBody,
		LatencyMs:        elapsed.Milliseconds(),
		TTFTMs:           res.ttft.Milliseconds(),
		Tps:              streamTPS(res.ttft > 0, res.status, res.usage, res.ttft, elapsed),
		PromptTokens:     res.usage.prompt,
		CompletionTokens: res.usage.completion,
	}
}

// cost 计费基准为 USD：CNY 定价模型按快照汇率折算入库，保证跨币种模型聚合一致。
// 只有成功请求计费：失败/断流可能已解析到半截 usage（Anthropic message_start 带 input_tokens），
// 按量计也不能入账。billing_mode=per_call 时忽略 token 用量，仅成功调用记 per_call_price。
// billing_mode=token（缺省/历史行）按 token 计价：缓存命中 token 单价取 cached_price，
// 未配置（<=0）回退输入价——OpenAI 系 prompt_tokens 本就把命中量包含在内，历史上即按输入价计费，回退保持兼容。
// 落库口径统一为 inclusive：prompt 含 cache_read 与 cache_creation，计费时先扣除再按各自单价。
// cache_creation 默认按输入价 × 1.25（Anthropic 5m cache write 官方倍率）；估算 usage 不计费。
func cost(m store.Model, u usageInfo, usdCNY float64, status string) float64 {
	if status != "success" || u.estimated {
		return 0
	}
	var raw float64
	switch m.BillingMode {
	case "per_call":
		raw = m.PerCallPrice
	case "audio_second":
		if u.seconds <= 0 {
			return 0
		}
		raw = u.seconds * m.AudioSecPrice
	case "char":
		if u.chars <= 0 {
			return 0
		}
		raw = float64(u.chars) * m.CharPrice / 1e6
	default:
		prompt := u.prompt - u.cached - u.cacheWrite
		if prompt < 0 {
			prompt = 0
		}
		cachedPrice := m.CachedPrice
		if cachedPrice <= 0 {
			cachedPrice = m.InputPrice
		}
		cacheWritePrice := m.InputPrice * 1.25
		raw = (float64(prompt)*m.InputPrice + float64(u.cached)*cachedPrice +
			float64(u.cacheWrite)*cacheWritePrice +
			float64(u.completion)*m.OutputPrice) / 1e6
	}
	if m.PriceCurrency == "CNY" {
		if usdCNY <= 0 {
			usdCNY = 7.25
		}
		return raw / usdCNY
	}
	return raw
}

const errorBodyLimit = 2048

func captureErrBody(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if len(b) > errorBodyLimit {
		return string(b[:errorBodyLimit]) + "…"
	}
	return string(b)
}

// record 按尝试结果做失败归因处置（§5.1）：
// 成功→清零组合禁用；401/403→永久禁用该组合（不影响其他模型/密钥）；
// 429→组合级短冷却（Retry-After 优先，不计入熔断）；
// 超时/5xx/连接/断流→组合级阶梯熔断，连续失败达阈值永久禁用；
// 客户端错误（400 等）与 client_disconnected 不属于上游故障，不记录。
func (h *Handler) record(res attemptResult, rt *config.Runtime) {
	if res.att.Model.ID == 0 || res.att.Key.ID == 0 {
		return
	}
	switch {
	case res.status == "success":
		h.rec.RecordModelKeySuccess(res.att.Model.ID, res.att.Key.ID)
	case res.errCode == "401" || res.errCode == "403":
		h.rec.RecordModelKeyFailure(res.att.Model.ID, res.att.Key.ID, res.errCode, false, rt)
	case res.errCode == "429":
		h.rec.RecordModelKeyRateLimited(res.att.Model.ID, res.att.Key.ID, res.retryAfterS, rt.RetryCooldownS)
	case res.retryable || res.streamBroke:
		h.rec.RecordModelKeyFailure(res.att.Model.ID, res.att.Key.ID, res.errCode, true, rt)
	}
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

type captureWriter struct {
	w                http.ResponseWriter
	mu               sync.Mutex
	buf              []byte
	limit            int
	overflow         bool
	clientReqHeaders string // 入站请求头快照（客户端 → OmniGate，格式化+脱敏）
	clientReqBody    string // 入站请求体快照（客户端原始提交，未修改）
	reqURL           string // 出站请求完整 URL（OmniGate → 上游，query 密钥参数脱敏）；最终 attempt 完成后回填
	reqHeaders       string // 出站请求头快照（OmniGate → 上游，格式化+脱敏）；最终 attempt 完成后回填
	reqBody          string // 出站请求体快照（最终 attempt 实际发送的内容）；随响应体一并落 content_log
	respHeaders      string // 上游响应头快照（格式化+脱敏）；attempt 完成后回填
	skipBody         bool   // TTS 二进制/SSE 不落 response_body，避免把音频塞进 SQLite
	bodyNote         string // skipBody 时写入 content_log 的占位摘要
}

func newCaptureWriter(w http.ResponseWriter, limit int) *captureWriter {
	return &captureWriter{w: w, limit: limit}
}

// setAttempt 回填该次 attempt 的出站请求快照与上游响应头；重试多次时后调用者覆盖（记录最终落点）。
func (cw *captureWriter) setAttempt(res attemptResult) {
	if cw == nil {
		return
	}
	cw.reqURL = res.reqURL
	cw.reqHeaders = res.reqHeaders
	cw.reqBody = string(res.reqBody)
	cw.respHeaders = formatHeaders(res.respHeaders)
}
func (cw *captureWriter) skipResponseBody(note string) {
	if cw == nil {
		return
	}
	cw.skipBody = true
	cw.bodyNote = note
}

// setClientReq 回填客户端入站请求快照（客户端 → OmniGate 的原始数据，未做任何修改）。
func (cw *captureWriter) setClientReq(h http.Header, body []byte) {
	if cw == nil {
		return
	}
	cw.clientReqHeaders = formatHeaders(h)
	cw.clientReqBody = string(body)
}

// maybeCapture 落 content_log：客户端入站请求、出站请求（OmniGate → 上游）与上游响应。
func (h *Handler) maybeCapture(requestID, route string, cw *captureWriter) {
	if cw == nil {
		return
	}
	respBody := cw.Body()
	err := h.db.DB.Exec(`
INSERT INTO content_log (request_id, route, client_request_headers, client_request_body, request_url, request_headers, request_body, response_headers, response_body, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(request_id) DO UPDATE SET
  route=excluded.route,
  client_request_headers=excluded.client_request_headers,
  client_request_body=excluded.client_request_body,
  request_url=excluded.request_url,
  request_headers=excluded.request_headers,
  request_body=excluded.request_body,
  response_headers=excluded.response_headers,
  response_body=excluded.response_body,
  created_at=excluded.created_at`,
		requestID, route, cw.clientReqHeaders, cw.clientReqBody, cw.reqURL, cw.reqHeaders, cw.reqBody, cw.respHeaders, respBody, time.Now().Unix(),
	).Error
	if err != nil {
		slog.Warn("write content_log failed", "err", err, "request_id", requestID)
	}
}

func (cw *captureWriter) Header() http.Header { return cw.w.Header() }

func (cw *captureWriter) WriteHeader(code int) { cw.w.WriteHeader(code) }

func (cw *captureWriter) Write(b []byte) (int, error) {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	if !cw.skipBody {
		if cw.overflow {
			return cw.w.Write(b)
		}
		if len(cw.buf)+len(b) <= cw.limit {
			cw.buf = append(cw.buf, b...)
		} else {
			cw.overflow = true
			cw.buf = append([]byte(nil), fmt.Sprintf("[truncated: response exceeds %d bytes]", cw.limit)...)
		}
	}
	return cw.w.Write(b)
}
func (cw *captureWriter) Flush() {
	if f, ok := cw.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (cw *captureWriter) Body() string {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	if cw.skipBody && cw.bodyNote != "" {
		return cw.bodyNote
	}
	return string(cw.buf)
}

// sensitiveHeaderFragments 命中任一子串（小写比较）的头视为敏感，值需脱敏。
var sensitiveHeaderFragments = []string{"auth", "key", "token", "secret", "cookie"}

func isSensitiveHeader(k string) bool {
	kl := strings.ToLower(k)
	for _, frag := range sensitiveHeaderFragments {
		if strings.Contains(kl, frag) {
			return true
		}
	}
	return false
}

// maskHeaderValue 保留前 8 个字符供辨认，其余以 **** 隐藏。
func maskHeaderValue(v string) string {
	if len(v) <= 8 {
		return "****"
	}
	return v[:8] + "****"
}

// captureURL 出站请求的完整 URL（scheme://host/path?query），供内容捕获展示"请求发往哪里"。
// query 命中敏感片段（key/token/auth 等，与请求头同一口径）的参数值脱敏：
// 部分提供商把凭据放在查询串（?api-key=xxx），落库不得出现明文密钥。
func captureURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	if u.RawQuery == "" {
		return u.String()
	}
	// 逐参数改写原始查询串：只替换敏感参数的值，其余部分原样保留（不重排顺序、不重新转义）。
	parts := strings.Split(u.RawQuery, "&")
	for i, p := range parts {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		name := k
		if dec, err := url.QueryUnescape(k); err == nil {
			name = dec
		}
		if !isSensitiveHeader(name) {
			continue
		}
		parts[i] = k + "=" + maskHeaderValue(v)
	}
	clone := *u
	clone.RawQuery = strings.Join(parts, "&")
	return clone.String()
}

// formatHeaders 将 HTTP 头格式化为 "Key: value" 多行文本（键排序保证稳定输出）。
func formatHeaders(h http.Header) string {
	if len(h) == 0 {
		return ""
	}
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		v := h.Get(k)
		if isSensitiveHeader(k) {
			v = maskHeaderValue(v)
		}
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(v)
		b.WriteString("\n")
	}
	return b.String()
}

// Messages 实现 Anthropic 原生端点 /v1/messages（直通模式）。
// 只路由到 protocol=messages 的模型，请求体不做转换直接透传。
func (h *Handler) Messages(w http.ResponseWriter, r *http.Request) {
	h.nativeEndpoint(w, r, "messages")
}

// Responses 实现 OpenAI Responses 原生端点 /v1/responses（直通模式）。
// 只路由到 protocol=responses 的模型，请求体不做转换直接透传。
func (h *Handler) Responses(w http.ResponseWriter, r *http.Request) {
	h.nativeEndpoint(w, r, "responses")
}

// nativeEndpoint 原生协议端点的通用处理逻辑：解析请求 → 协议过滤 → 直通转发。
// 协议过滤在 Selector.pick 内按路由 endpoint 完成（messages/responses 只挑同协议模型）。
func (h *Handler) nativeEndpoint(w http.ResponseWriter, r *http.Request, endpoint string) {
	start := time.Now()
	requestID := newRequestID()
	w.Header().Set("X-Request-Id", requestID)

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		openAIError(w, 400, errReadFailed, "failed to read request body", nil)
		return
	}
	if len(body) > maxBodyBytes {
		openAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 32MB", nil)
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		openAIError(w, 400, "invalid_json", "request body is not valid JSON", nil)
		return
	}

	// 从请求体提取 model 字段（Anthropic/Responses 都在顶层）
	routeName, _ := req["model"].(string)
	if routeName == "" {
		openAIError(w, 400, "missing_model", "request body must contain a model field", nil)
		return
	}
	isStream, _ := req["stream"].(bool)

	rt := h.rt.Snapshot()
	captureOn := rt.CaptureEnabled && (len(rt.CaptureRoutes) == 0 || containsStr(rt.CaptureRoutes, routeName))
	var cw *captureWriter
	if captureOn {
		cw = newCaptureWriter(w, 1<<20)
		w = cw
		cw.setClientReq(r.Header, body)
	}

	snap, found, err := h.sel.LoadSnapshot(routeName)
	if err != nil {
		slog.Error("load snapshot failed", "err", err, "route", routeName, "endpoint", endpoint)
		openAIError(w, 500, "internal_error", "failed to load routing config", nil)
		h.maybeCapture(requestID, routeName, cw)
		return
	}
	if !found {
		openAIError(w, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("the model '%s' does not exist", routeName), nil)
		h.maybeCapture(requestID, routeName, cw)
		return
	}

	if snap.Route.Endpoint != endpoint {
		openAIError(w, http.StatusBadRequest, "endpoint_mismatch",
			fmt.Sprintf("route '%s' is configured for endpoint '%s', but you called '%s'", routeName, snap.Route.Endpoint, endpoint), nil)
		h.maybeCapture(requestID, routeName, cw)
		return
	}

	var vkID int64
	if vk, ok := getVKFromContext(r.Context()); ok {
		vkID = vk.ID
		if err := checkVKRouteAccess(h.db, vk, snap.Route.ID); err != nil {
			if err == store.ErrVKAccessDenied {
				openAIError(w, 403, "route_denied", fmt.Sprintf("route '%s' not allowed by virtual key", routeName), nil)
			} else {
				openAIError(w, 500, "route_check_error", err.Error(), nil)
			}
			h.maybeCapture(requestID, routeName, cw)
			return
		}
	}
	pendingID := h.createPendingLog(requestID, routeName, endpoint, isStream, vkID)
	tried := map[router.Combo]bool{}
	maxAttempts := rt.BreakerMaxHops + 1
	var last attemptResult
	var errCodes []string
	priorFails := 0
	var attempts []store.RequestAttempt
	finalLogged := false

	useFallback := func() bool {
		fbID := snap.Route.FallbackModelID
		if fbID <= 0 {
			return false
		}
		fallbackAtt, fallbackOK := h.sel.PickFallback(fbID, time.Now())
		if !fallbackOK {
			slog.Warn("fallback model unavailable", "route", routeName, "fallback_model_id", fbID, "endpoint", endpoint)
			return false
		}
		slog.Info("using fallback model", "route", routeName, "fallback_model_id", fbID, "endpoint", endpoint)
		attemptStart := time.Now()
		res := h.nativeAttempt(w, r, body, fallbackAtt, isStream, rt, endpoint)
		res.latencyMs, res.elapsed = time.Since(attemptStart).Milliseconds(), time.Since(attemptStart)
		h.record(res, rt)
		attempts = append(attempts, h.attemptRow(requestID, routeName, len(attempts), fallbackAtt, res, attemptStart))
		h.writeLog(start, requestID, routeName, fallbackAtt, isStream,
			res.status, res.errCode, res.usage, res.ttft, time.Since(start), priorFails, res.errorBody, true, vkID, pendingID, attempts)
		cw.setAttempt(res)
		h.maybeCapture(requestID, routeName, cw)
		return true
	}

	for attempt := range maxAttempts {
		att, ok := h.sel.Pick(snap, tried, time.Now(), 0)
		if !ok {
			if useFallback() {
				return
			}
			if attempt == 0 {
				attempts = append(attempts, h.attemptRow(requestID, routeName, 0, router.Attempt{}, attemptResult{
					status:  "error",
					errCode: errAllBackendsUnavailable,
				}, start))
				statuses := h.sel.BackendStatuses(snap, time.Now())
				h.writeLog(start, requestID, routeName, router.Attempt{}, isStream,
					"error", errAllBackendsUnavailable, usageInfo{}, 0, time.Since(start), priorFails, "", false, vkID, pendingID, attempts)
				openAIError(w, http.StatusServiceUnavailable, errAllBackendsUnavailable,
					fmt.Sprintf("route '%s' has no available backends", routeName), statuses)
				h.maybeCapture(requestID, routeName, cw)
				return
			}
			break
		}

		tried[att.Combo()] = true
		attemptStart := time.Now()
		res := h.nativeAttempt(w, r, body, att, isStream, rt, endpoint)
		res.latencyMs, res.elapsed = time.Since(attemptStart).Milliseconds(), time.Since(attemptStart)
		h.record(res, rt)
		attempts = append(attempts, h.attemptRow(requestID, routeName, attempt, att, res, attemptStart))
		last = res
		if res.committed || !res.retryable {
			h.writeLog(start, requestID, routeName, att, isStream,
				res.status, res.errCode, res.usage, res.ttft, time.Since(start), priorFails, res.errorBody, false, vkID, pendingID, attempts)
			finalLogged = true
			break
		}
		priorFails++
		errCodes = append(errCodes, res.errCode)
		slog.Warn("attempt failed, transferring", "route", routeName,
			"model", att.Model.Name, "key_id", att.Key.ID, "code", res.errCode, "endpoint", endpoint)
	}

	if !finalLogged {
		if useFallback() {
			return
		}
		if last.att.Model.ID != 0 && !last.committed && last.retryable {
			h.writeLog(start, requestID, routeName, last.att, isStream,
				last.status, last.errCode, last.usage, last.ttft, time.Since(start), priorFails-1, last.errorBody, false, vkID, pendingID, attempts)
		}
	}

	if !last.committed {
		openAIError(w, http.StatusBadGateway, errAllRetriesFailed,
			fmt.Sprintf("all attempts failed after %d retries (error sequence: %s)", priorFails, strings.Join(errCodes, " → ")), nil)
		h.maybeCapture(requestID, routeName, cw)
		return
	}
	if last.att.Model.ID != 0 {
		cw.setAttempt(last)
	}
	h.maybeCapture(requestID, routeName, cw)
}

// nativeAttempt 原生协议的单次转发尝试（请求体直通，不做协议转换）。
func (h *Handler) nativeAttempt(w http.ResponseWriter, r *http.Request, reqBody []byte,
	att router.Attempt, isStream bool, rt *config.Runtime, endpoint string) attemptResult {

	attemptStart := time.Now()
	res := attemptResult{att: att, promptChars: nativeTextChars(reqBody)}
	adapter := AdapterFor(att.Model.Protocol)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var timedOut atomic.Bool
	deadline := time.AfterFunc(providerTimeout(att.Provider.TimeoutMs), func() {
		timedOut.Store(true)
		cancel()
	})
	defer deadline.Stop()
	stopDeadline := func() { deadline.Stop() }
	reqBody = rewriteJSONModel(reqBody, att.Model.Name)

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, adapter.endpoint(att.Provider.BaseURL, &att.Model), bytes.NewReader(reqBody))
	if err != nil {
		res.errCode, res.status = errBadUpstreamURL, "error"
		return res
	}
	upReq.Header.Set("Content-Type", "application/json")
	extra := map[string]string{}
	adapter.setHeaders(extra, att.Key.KeyValue)
	for k, v := range extra {
		upReq.Header.Set(k, v)
	}
	ApplyUpstreamIdentity(upReq, att.Provider, r)
	if isStream {
		upReq.Header.Set("Accept", "text/event-stream")
	}
	// 出站快照：内容捕获（content_log）记录的是 OmniGate → 上游的实际请求（头已含模拟/认证头）
	res.reqURL = captureURL(upReq.URL)
	res.reqHeaders = formatHeaders(upReq.Header)
	res.reqBody = reqBody

	resp, err := h.clientForProvider(att.Provider.ID).Do(upReq)
	if err != nil {
		res.retryable, res.status = true, "error"
		if timedOut.Load() || errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
			res.errCode = errTimeout
		} else {
			res.errCode = errConnectionFailed
		}
		return res
	}
	res.respHeaders = resp.Header
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		res.httpStatus = resp.StatusCode
		if isStream {
			return h.nativeStreamResponse(w, resp, att, attemptStart, cancel, rt, adapter, res, stopDeadline, &timedOut, r.Context())
		}
		return h.nativeBufferedResponse(w, resp, attemptStart, adapter, res, &timedOut)
	}

	res.httpStatus = resp.StatusCode
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	res.errCode = strconv.Itoa(resp.StatusCode)
	res.errorBody = captureErrBody(errBody)
	if resp.StatusCode == 429 {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, convErr := strconv.Atoi(ra); convErr == nil && secs > 0 {
				res.retryAfterS = secs
			}
		}
	}
	retryable := false
	for _, code := range rt.RetryableStatuses {
		if resp.StatusCode == code {
			retryable = true
			break
		}
	}
	if retryable {
		res.retryable, res.status = true, "error"
		return res
	}
	res.committed, res.status = true, "client_error"
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(errBody)
	return res
}

// nativeBufferedResponse 处理原生协议的非流式响应。
func (h *Handler) nativeBufferedResponse(w http.ResponseWriter, resp *http.Response,
	attemptStart time.Time, adapter ProtocolAdapter, res attemptResult, timedOut *atomic.Bool) attemptResult {

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		if timedOut.Load() {
			res.errCode, res.status, res.retryable = errTimeout, "error", true
		} else {
			res.errCode, res.status, res.retryable = errReadFailed, "error", true
		}
		return res
	}

	// 原生协议不转换，直接透传
	out := body

	// 尝试解析 usage（不同协议格式不同）
	converted, usage, _ := adapter.convertBuffered(body)
	if usage.prompt > 0 || usage.completion > 0 {
		res.usage = usage
	} else if len(converted) > 0 {
		// 从转换后的 OpenAI 格式中提取 usage
		var parsed struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(converted, &parsed)
		if parsed.Usage != nil {
			res.usage = usageInfo{prompt: parsed.Usage.PromptTokens, completion: parsed.Usage.CompletionTokens}
		}
	}

	res.committed, res.status, res.ttft = true, "success", time.Since(attemptStart)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Modelrouter-Model", res.att.Model.Name)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
	return res
}

// nativeStreamResponse 处理原生协议的流式响应（直通 SSE）。
func (h *Handler) nativeStreamResponse(w http.ResponseWriter, resp *http.Response, att router.Attempt,
	attemptStart time.Time, cancel context.CancelFunc, rt *config.Runtime,
	adapter ProtocolAdapter, res attemptResult, stopDeadline func(), timedOut *atomic.Bool, clientCtx context.Context) attemptResult {

	idle := newIdleReader(resp.Body, time.Duration(rt.StreamIdleTimeoutS)*time.Second, cancel)
	defer idle.Close()
	buf := make([]byte, 32<<10)
	flusher, _ := w.(http.Flusher)
	committed := false
	splitter := &sseSplitter{}
	scan := newSSEScan()

	applyNativeUsage := func() {
		if u := adapter.streamUsage(); u != nil {
			res.usage = *u
			return
		}
		scan.Finish()
		if scan.Usage() != nil {
			cached := 0
			if scan.Usage().PromptTokensDetails != nil {
				cached = scan.Usage().PromptTokensDetails.CachedTokens
			}
			res.usage = usageInfo{prompt: scan.Usage().PromptTokens, completion: scan.Usage().CompletionTokens, cached: cached}
		}
	}

	for {
		n, readErr := idle.Read(buf)
		if n > 0 {
			if !committed {
				committed = true
				stopDeadline()
				res.committed = true
				res.ttft = time.Since(attemptStart)
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("X-Modelrouter-Model", att.Model.Name)
				w.WriteHeader(http.StatusOK)
			}
			scan.Write(buf[:n])
			for _, payload := range splitter.write(buf[:n]) {
				_ = adapter.convertStreamChunk(payload)
			}
			if _, wErr := w.Write(buf[:n]); wErr != nil {
				applyNativeUsage()
				res.status, res.errCode = "error", errClientDisconnected
				return res
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			for _, payload := range splitter.flush() {
				_ = adapter.convertStreamChunk(payload)
			}
			applyNativeUsage()
			if !committed {
				if errors.Is(readErr, io.EOF) {
					res.status, res.errCode, res.retryable = "error", errEmptyStream, true
				} else if timedOut.Load() {
					res.errCode, res.status, res.retryable = errTimeout, "error", true
				} else {
					res.errCode, res.status, res.retryable = errStreamSetupFailed, "error", true
				}
				return res
			}
			complete := adapter.streamComplete() || scan.Done()
			if res.usage.prompt == 0 && res.usage.completion == 0 && complete {
				res.usage = estimateUsage(res.promptChars, "")
			}
			if errors.Is(readErr, io.EOF) {
				if complete {
					res.status = "success"
				} else {
					res.status, res.errCode, res.streamBroke = "error", errStreamBroken, true
				}
				return res
			}
			if clientCtx != nil && clientCtx.Err() != nil {
				if complete {
					res.status = "success"
				} else {
					res.status, res.errCode = "error", errClientDisconnected
				}
				return res
			}
			if complete {
				res.status = "success"
				return res
			}
			res.status, res.errCode, res.streamBroke = "error", errStreamBroken, true
			return res
		}
	}
}

// rewriteJSONModel 仅替换顶层 model 字段，其余 JSON 结构保持语义不变。
// 解析失败时原样返回，避免把非法体再破坏一次。
func rewriteJSONModel(body []byte, model string) []byte {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return body
	}
	if cur, _ := req["model"].(string); cur == model {
		return body
	}
	req["model"] = model
	out, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return out
}
