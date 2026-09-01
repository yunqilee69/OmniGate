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
		proxyURL, err := url.Parse(p.ProxyURL)
		if err == nil {
			transport := &http.Transport{
				Proxy:               http.ProxyURL(proxyURL),
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
			}
			client = &http.Client{Transport: transport}
		}
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
	cached     int
	estimated  bool
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
	retryAfterS int
	streamBroke bool
	errorBody   string // 上游错误响应体摘要（< 2KB）；仅错误路径填充
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
		openAIError(w, 400, "read_error", "failed to read request body", nil)
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

	rt := h.rt.Snapshot()
	captureOn := rt.CaptureEnabled && (len(rt.CaptureRoutes) == 0 || containsStr(rt.CaptureRoutes, routeName))
	var cw *captureWriter
	var reqSnap string
	if captureOn {
		reqSnap = string(body)
		cw = newCaptureWriter(w, 1<<20)
		w = cw
	}

	var affKey string
	var affModel int64
	if rt.AffinityEnabled {
		if sk := sessionKey(r, req, rt.AffinityHeaders); sk != "" {
			affKey = routeName + "\x00" + sk
			affModel, _ = h.sel.Affinity(affKey, time.Now())
		}
	}

	snap, found, err := h.sel.LoadSnapshot(routeName)
	if err != nil {
		slog.Error("load snapshot failed", "err", err, "route", routeName)
		openAIError(w, 500, "internal_error", "failed to load routing config", nil)
		h.maybeCapture(requestID, routeName, reqSnap, cw)
		return
	}
	if !found {
		openAIError(w, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("the model '%s' does not exist", routeName), nil)
		h.maybeCapture(requestID, routeName, reqSnap, cw)
		return
	}

	tried := map[int64]bool{}
	maxAttempts := rt.BreakerMaxHops + 1
	var last attemptResult
	var errCodes []string
	priorFails := 0

	for attempt := 0; attempt < maxAttempts; attempt++ {
		att, ok := h.sel.Pick(snap, tried, time.Now(), affModel)
		if !ok {
			if attempt == 0 {
				if rt.FallbackEnabled && rt.FallbackModelID > 0 {
					fallbackAtt, fallbackOK := h.sel.PickFallback(rt.FallbackModelID, time.Now())
					if fallbackOK {
						slog.Info("using fallback model", "route", routeName, "fallback_model_id", rt.FallbackModelID)
						attemptStart := time.Now()
						res := h.attempt(w, r, req, fallbackAtt, isStream, rt)
						res.latencyMs = time.Since(attemptStart).Milliseconds()
						h.record(res, rt)
						h.writeLog(start, requestID, routeName, fallbackAtt, isStream,
							res.status, res.errCode, res.usage, res.ttft, time.Since(start), 0, res.errorBody, true)
						h.maybeCapture(requestID, routeName, reqSnap, cw)
						return
					}
					slog.Warn("fallback model unavailable", "route", routeName, "fallback_model_id", rt.FallbackModelID)
				}

				statuses := h.sel.BackendStatuses(snap, time.Now())
				h.writeLog(start, requestID, routeName, router.Attempt{}, isStream,
					"error", "all_backends", usageInfo{}, 0, time.Since(start), priorFails, "", false)
				openAIError(w, http.StatusServiceUnavailable, "all_backends_unavailable",
					fmt.Sprintf("route '%s' has no available backends", routeName), statuses)
				h.maybeCapture(requestID, routeName, reqSnap, cw)
				return
			}
			break
		}
		tried[att.Key.ID] = true
		attemptStart := time.Now()
		res := h.attempt(w, r, req, att, isStream, rt)
		res.latencyMs = time.Since(attemptStart).Milliseconds()
		h.record(res, rt)
		h.writeAttempt(requestID, routeName, attempt, att, res, attemptStart)
		last = res
		// 只在最后一次记录 request_log（committed 或不可重试时）
		if res.committed || !res.retryable {
			h.writeLog(start, requestID, routeName, att, isStream,
				res.status, res.errCode, res.usage, res.ttft, time.Since(start), priorFails, res.errorBody, false)
			break
		}
		priorFails++
		errCodes = append(errCodes, res.errCode)
		slog.Warn("attempt failed, transferring", "route", routeName,
			"model", att.Model.Name, "key_id", att.Key.ID, "code", res.errCode)
	}

	// 如果循环结束但没有记录日志（所有尝试都失败且可重试），记录最后一次的结果
	if last.att.Model.ID != 0 && !last.committed && last.retryable {
		h.writeLog(start, requestID, routeName, last.att, isStream,
			last.status, last.errCode, last.usage, last.ttft, time.Since(start), priorFails, last.errorBody, false)
	}

	// 亲和只在最终成功后回写：失败转移到别的模型成功时，记住的是缓存真正生效的落点。
	if affKey != "" && last.att.Model.ID != 0 && last.status == "success" {
		h.sel.SetAffinity(affKey, last.att.Model.ID, rt.AffinityTTL, time.Now())
	}

	if len(errCodes) > 1 {
		slog.Info("all attempts exhausted", "route", routeName,
			"attempts", len(errCodes), "errors", errCodes)
	}
	h.maybeCapture(requestID, routeName, reqSnap, cw)
}

func (h *Handler) attempt(w http.ResponseWriter, r *http.Request, req map[string]any,
	att router.Attempt, isStream bool, rt *config.Runtime) attemptResult {

	attemptStart := time.Now()
	res := attemptResult{att: att, promptChars: requestTextChars(req)}
	adapter := AdapterFor(att.Model.Protocol)

	// deadline 只约束「建连 + 首字节」：流式首字节到达即停表（stopDeadline），
	// 之后流的生命周期交给 idle reader 的空闲超时，长输出流不会被整体截断；
	// 非流式的响应完成等价于首字节，不停表即覆盖整个响应。
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var timedOut atomic.Bool
	deadline := time.AfterFunc(time.Duration(att.Provider.TimeoutMs)*time.Millisecond, func() {
		timedOut.Store(true)
		cancel()
	})
	defer deadline.Stop() // attempt 返回后不再需要定时器（流式首字节处已提前停表）
	stopDeadline := func() { deadline.Stop() }

	req["model"] = att.Model.Name
	converted, err := adapter.buildBody(req)
	if err != nil {
		res.errCode, res.status = "protocol_convert_error", "error"
		return res
	}
	if isStream && att.Model.Protocol == "openai" && rt.StreamInjectUsage {
		so, _ := converted["stream_options"].(map[string]any)
		if so == nil {
			so = map[string]any{}
		}
		so["include_usage"] = true
		converted["stream_options"] = so
	}
	outBody, err := json.Marshal(converted)
	if err != nil {
		res.errCode, res.status = "marshal_error", "error"
		return res
	}

	// Debug: 记录即将发送的请求
	if rt.DebugStreamLog {
		slog.Info("[DEBUG] Outbound request to upstream",
			"provider", att.Provider.Name,
			"model", att.Model.Name,
			"key_id", att.Key.ID,
			"protocol", att.Model.Protocol,
			"endpoint", adapter.endpoint(att.Provider.BaseURL),
			"timeout_ms", att.Provider.TimeoutMs,
			"is_stream", isStream,
			"request_body", string(outBody))
	}

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, adapter.endpoint(att.Provider.BaseURL), bytes.NewReader(outBody))
	if err != nil {
		res.errCode, res.status = "bad_upstream_url", "error"
		return res
	}
	upReq.Header.Set("Content-Type", "application/json")
	extra := map[string]string{}
	adapter.setHeaders(extra, att.Key.KeyValue)
	for k, v := range extra {
		upReq.Header.Set(k, v)
	}
	if isStream {
		upReq.Header.Set("Accept", "text/event-stream")
	}

	// Debug: 记录请求头（脱敏）
	if rt.DebugStreamLog {
		headers := make(map[string]string)
		for k := range upReq.Header {
			v := upReq.Header.Get(k)
			// 脱敏敏感头
			if strings.Contains(strings.ToLower(k), "auth") || strings.Contains(strings.ToLower(k), "key") {
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
			res.errCode = "timeout"
		} else {
			res.errCode = "conn"
		}
		return res
	}
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
			return h.streamResponse(w, resp, att, attemptStart, cancel, rt, adapter, res, stopDeadline, &timedOut)
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
			res.errCode, res.status, res.retryable = "timeout", "error", true
		} else {
			res.errCode, res.status, res.retryable = "read_error", "error", true
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
		res.errCode, res.status, res.retryable = "convert_error", "error", true
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
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
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
	adapter ProtocolAdapter, res attemptResult, stopDeadline func(), timedOut *atomic.Bool) attemptResult {

	idle := newIdleReader(resp.Body, time.Duration(rt.StreamIdleTimeoutS)*time.Second, cancel)
	defer idle.Close()
	passthrough := att.Model.Protocol == "openai"
	scan := newSSEScan()
	splitter := &sseSplitter{}
	var textAcc strings.Builder
	buf := make([]byte, 32<<10)
	flusher, _ := w.(http.Flusher)
	committed := false

	writeToClient := func(lines []string) bool {
		for _, ln := range lines {
			if _, wErr := w.Write([]byte(ln + "\n\n")); wErr != nil {
				res.status, res.errCode = "error", "client_disconnected"
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
					res.status, res.errCode = "error", "client_disconnected"
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
			// 先判断错误类型和最终状态，再决定是否记录日志
			if errors.Is(readErr, io.EOF) {
				if !passthrough {
					for _, payload := range splitter.flush() {
						if !writeToClient(adapter.convertStreamChunk(payload)) {
							return res
						}
					}
					if !writeToClient(adapter.streamFinal()) {
						return res
					}
				} else {
					scan.Finish()
				}
				if !committed {
					res.status, res.errCode, res.retryable = "error", "empty_stream", true
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
				res.status = "success"
				return res
		}
		// 已 committed 但遇到非 EOF 错误（如 context canceled）
		if !committed {
			// deadline 在首字节前触发会 cancel 流：区分超时与上游建连失败
			if timedOut.Load() {
				res.status, res.errCode, res.retryable = "error", "timeout", true
			} else {
				res.status, res.errCode, res.retryable = "error", "stream_setup_failed", true
			}
			return res
		}
		
		// 流已 committed 且遇到错误：尝试提取真实 usage，并判断是否为成功后的客户端断开
		if passthrough {
			scan.Finish() // 确保解析完所有已收到的数据
		}
		
		// 尝试获取真实 usage
		if u := adapter.streamUsage(); u != nil {
			res.usage = *u
		} else if passthrough && scan.Usage() != nil {
			// passthrough 模式下从 scan 中获取真实 token
			cached := 0
			if scan.Usage().PromptTokensDetails != nil {
				cached = scan.Usage().PromptTokensDetails.CachedTokens
			}
			res.usage = usageInfo{
				prompt:     scan.Usage().PromptTokens,
				completion: scan.Usage().CompletionTokens,
				cached:     cached,
				estimated:  false, // 真实值，非估算
			}
		} else {
			// 无法获取真实 usage，使用估算
			if passthrough {
				textAcc.WriteString(scan.Text())
			}
			res.usage = estimateUsage(res.promptChars, textAcc.String())
		}
		
		// 判断：如果已获得 usage（说明流传输完整），将 context canceled 视为成功
		// 典型场景：客户端收到 [DONE] 后立即关闭连接
		if res.usage.prompt > 0 || res.usage.completion > 0 {
			res.status = "success"
		} else {
			// usage 缺失，确实是流中断
			res.status, res.errCode, res.streamBroke = "error", "stream_broken", true
			// 只记录真正的错误
			if h.rt.Snapshot().DebugStreamLog {
				slog.Info("[DEBUG] Stream broken",
					"model", att.Model.Name,
					"provider", att.Provider.Name,
					"error", readErr.Error(),
					"committed", committed)
			}
		}
		return res
		}
	}
}

func (h *Handler) writeLog(start time.Time, requestID, routeName string, att router.Attempt,
	isStream bool, status, errCode string, u usageInfo, ttft, total time.Duration,
	retries int, errorBody string, isFallback bool) {

	entry := store.RequestLog{
		RequestID: requestID, Route: routeName,
		Status: status, ErrorCode: errCode, ErrorBody: errorBody, IsStream: isStream,
		IsFallback:   isFallback,
		PromptTokens: u.prompt, CompletionTokens: u.completion, CachedTokens: u.cached, TokensEstimated: u.estimated,
		TTFTMs: ttft.Milliseconds(), TotalMs: total.Milliseconds(),
		Cost: cost(att.Model, u, h.rt.Snapshot().USDCNY), Retries: retries,
	}
	if att.Model.ID != 0 {
		entry.Model = att.Model.Name
		entry.Provider = att.Provider.Name
		entry.KeyID = att.Key.ID
	}
	if err := h.db.DB.Create(&entry).Error; err != nil {
		slog.Error("write request_log failed", "err", err, "request_id", requestID)
		return
	}
	store.UpsertDaily(h.db.DB, &entry)
}

// writeAttempt 把每一次转发尝试的完整明细落库（含成功与失败），便于排查重试链路。
func (h *Handler) writeAttempt(requestID, routeName string, attempt int, att router.Attempt,
	res attemptResult, start time.Time) {
	if att.Model.ID == 0 {
		return
	}
	row := store.RequestAttempt{
		RequestID:        requestID,
		Route:            routeName,
		Attempt:          attempt,
		Model:            att.Model.Name,
		Provider:         att.Provider.Name,
		KeyID:            att.Key.ID,
		Status:           res.status,
		HTTPStatus:       res.httpStatus,
		ErrorCode:        res.errCode,
		ErrorBody:        res.errorBody,
		LatencyMs:        time.Since(start).Milliseconds(),
		TTFTMs:           res.ttft.Milliseconds(),
		PromptTokens:     res.usage.prompt,
		CompletionTokens: res.usage.completion,
	}
	if err := h.db.DB.Create(&row).Error; err != nil {
		slog.Error("write request_attempt failed", "err", err, "request_id", requestID)
	}
}

// cost 计费基准为 USD：CNY 定价模型按快照汇率折算入库，保证跨币种模型聚合一致。
func cost(m store.Model, u usageInfo, usdCNY float64) float64 {
	raw := float64(u.prompt)*m.InputPrice/1e6 + float64(u.completion)*m.OutputPrice/1e6
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
// 成功→清零；401/403→禁 key；429→冷却 key（Retry-After 优先）；超时/5xx/连接/断流→模型阶梯熔断；
// 客户端错误（400 等）与 client_disconnected 不属于上游故障，不记录。
func (h *Handler) record(res attemptResult, rt *config.Runtime) {
	if res.att.Model.ID == 0 || res.att.Key.ID == 0 {
		return
	}
	switch {
	case res.status == "success":
		h.rec.RecordModelSuccess(res.att.Model.ID)
		h.rec.RecordKeySuccess(res.att.Key.ID)
	case res.errCode == "401" || res.errCode == "403":
		h.rec.RecordKeyAuthFailure(res.att.Key.ID, res.errCode)
	case res.errCode == "429":
		h.rec.RecordKeyRateLimited(res.att.Key.ID, res.retryAfterS, rt.RetryCooldownS)
	case res.retryable || res.streamBroke:
		h.rec.RecordModelFailure(res.att.Model.ID, res.errCode, rt)
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
	w        http.ResponseWriter
	buf      []byte
	limit    int
	overflow bool
}

func newCaptureWriter(w http.ResponseWriter, limit int) *captureWriter {
	return &captureWriter{w: w, limit: limit}
}

func (cw *captureWriter) Header() http.Header { return cw.w.Header() }

func (cw *captureWriter) WriteHeader(code int) { cw.w.WriteHeader(code) }

func (cw *captureWriter) Write(b []byte) (int, error) {
	if cw.overflow {
		return cw.w.Write(b)
	}
	if len(cw.buf)+len(b) <= cw.limit {
		cw.buf = append(cw.buf, b...)
	} else {
		cw.overflow = true
		cw.buf = append([]byte(nil), fmt.Sprintf("[truncated: response exceeds %d bytes]", cw.limit)...)
	}
	return cw.w.Write(b)
}

func (cw *captureWriter) Body() string {
	return string(cw.buf)
}

func (h *Handler) maybeCapture(requestID, route, reqBody string, cw *captureWriter) {
	if cw == nil {
		return
	}
	respBody := cw.Body()
	err := h.db.DB.Exec(`
INSERT INTO content_log (request_id, route, request_body, response_body, created_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(request_id) DO UPDATE SET
  route=excluded.route,
  request_body=excluded.request_body,
  response_body=excluded.response_body,
  created_at=excluded.created_at`,
		requestID, route, reqBody, respBody, time.Now().Unix(),
	).Error
	if err != nil {
		slog.Warn("write content_log failed", "err", err, "request_id", requestID)
	}
}

// Messages 实现 Anthropic 原生端点 /v1/messages（直通模式）。
// 只路由到 protocol=anthropic 的模型，请求体不做转换直接透传。
func (h *Handler) Messages(w http.ResponseWriter, r *http.Request) {
	h.nativeEndpoint(w, r, "anthropic", "/v1/messages")
}

// Responses 实现 OpenAI Responses 原生端点 /v1/responses（直通模式）。
// 只路由到 protocol=responses 的模型，请求体不做转换直接透传。
func (h *Handler) Responses(w http.ResponseWriter, r *http.Request) {
	h.nativeEndpoint(w, r, "responses", "/v1/responses")
}

// nativeEndpoint 原生协议端点的通用处理逻辑：解析请求 → 协议过滤 → 直通转发。
func (h *Handler) nativeEndpoint(w http.ResponseWriter, r *http.Request, protocol, endpoint string) {
	start := time.Now()
	requestID := newRequestID()
	w.Header().Set("X-Request-Id", requestID)

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		openAIError(w, 400, "read_error", "failed to read request body", nil)
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
	var reqSnap string
	if captureOn {
		reqSnap = string(body)
		cw = newCaptureWriter(w, 1<<20)
		w = cw
	}

	snap, found, err := h.sel.LoadSnapshot(routeName)
	if err != nil {
		slog.Error("load snapshot failed", "err", err, "route", routeName, "endpoint", endpoint)
		openAIError(w, 500, "internal_error", "failed to load routing config", nil)
		h.maybeCapture(requestID, routeName, reqSnap, cw)
		return
	}
	if !found {
		openAIError(w, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("the model '%s' does not exist", routeName), nil)
		h.maybeCapture(requestID, routeName, reqSnap, cw)
		return
	}

	if snap.Route.Endpoint != endpoint {
		openAIError(w, http.StatusBadRequest, "endpoint_mismatch",
			fmt.Sprintf("route '%s' is configured for endpoint '%s', but you called '%s'", routeName, snap.Route.Endpoint, endpoint), nil)
		h.maybeCapture(requestID, routeName, reqSnap, cw)
		return
	}

	tried := map[int64]bool{}
	maxAttempts := rt.BreakerMaxHops + 1
	var last attemptResult
	var errCodes []string
	priorFails := 0

	for attempt := 0; attempt < maxAttempts; attempt++ {
		att, ok := h.sel.Pick(snap, tried, time.Now(), 0)
		if !ok {
			if attempt == 0 {
				if rt.FallbackEnabled && rt.FallbackModelID > 0 {
					fallbackAtt, fallbackOK := h.sel.PickFallback(rt.FallbackModelID, time.Now())
					if fallbackOK {
						slog.Info("using fallback model", "route", routeName, "fallback_model_id", rt.FallbackModelID, "endpoint", endpoint)
						attemptStart := time.Now()
						res := h.nativeAttempt(w, r, body, fallbackAtt, isStream, rt, endpoint)
						res.latencyMs = time.Since(attemptStart).Milliseconds()
						h.record(res, rt)
						h.writeLog(start, requestID, routeName, fallbackAtt, isStream,
							res.status, res.errCode, res.usage, res.ttft, time.Since(start), 0, res.errorBody, true)
						h.maybeCapture(requestID, routeName, reqSnap, cw)
						return
					}
					slog.Warn("fallback model unavailable", "route", routeName, "fallback_model_id", rt.FallbackModelID, "endpoint", endpoint)
				}

				statuses := h.sel.BackendStatuses(snap, time.Now())
				h.writeLog(start, requestID, routeName, router.Attempt{}, isStream,
					"error", "all_backends", usageInfo{}, 0, time.Since(start), priorFails, "", false)
				openAIError(w, http.StatusServiceUnavailable, "all_backends_unavailable",
					fmt.Sprintf("route '%s' has no available backends", routeName), statuses)
				h.maybeCapture(requestID, routeName, reqSnap, cw)
				return
			}
			break
		}

		tried[att.Key.ID] = true
		attemptStart := time.Now()
		res := h.nativeAttempt(w, r, body, att, isStream, rt, endpoint)
		res.latencyMs = time.Since(attemptStart).Milliseconds()
		h.record(res, rt)
		h.writeAttempt(requestID, routeName, attempt, att, res, attemptStart)
		last = res
		h.writeLog(start, requestID, routeName, att, isStream,
			res.status, res.errCode, res.usage, res.ttft, time.Since(start), priorFails, res.errorBody, false)
		if res.committed || !res.retryable {
			break
		}
		priorFails++
		errCodes = append(errCodes, res.errCode)
		slog.Warn("attempt failed, transferring", "route", routeName,
			"model", att.Model.Name, "key_id", att.Key.ID, "code", res.errCode, "endpoint", endpoint)
	}

	if !last.committed {
		openAIError(w, http.StatusBadGateway, "all_attempts_failed",
			fmt.Sprintf("all attempts failed after %d retries (error sequence: %s)", priorFails, strings.Join(errCodes, " → ")), nil)
		h.maybeCapture(requestID, routeName, reqSnap, cw)
		return
	}
	h.maybeCapture(requestID, routeName, reqSnap, cw)
}

// nativeAttempt 原生协议的单次转发尝试（请求体直通，不做协议转换）。
func (h *Handler) nativeAttempt(w http.ResponseWriter, r *http.Request, reqBody []byte,
	att router.Attempt, isStream bool, rt *config.Runtime, endpoint string) attemptResult {

	attemptStart := time.Now()
	res := attemptResult{att: att, promptChars: len(reqBody) / 4} // 粗略估算
	adapter := AdapterFor(att.Model.Protocol)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var timedOut atomic.Bool
	deadline := time.AfterFunc(time.Duration(att.Provider.TimeoutMs)*time.Millisecond, func() {
		timedOut.Store(true)
		cancel()
	})
	defer deadline.Stop()
	stopDeadline := func() { deadline.Stop() }

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, adapter.endpoint(att.Provider.BaseURL), bytes.NewReader(reqBody))
	if err != nil {
		res.errCode, res.status = "bad_upstream_url", "error"
		return res
	}
	upReq.Header.Set("Content-Type", "application/json")
	extra := map[string]string{}
	adapter.setHeaders(extra, att.Key.KeyValue)
	for k, v := range extra {
		upReq.Header.Set(k, v)
	}
	if isStream {
		upReq.Header.Set("Accept", "text/event-stream")
	}

	resp, err := h.clientForProvider(att.Provider.ID).Do(upReq)
	if err != nil {
		res.retryable, res.status = true, "error"
		if timedOut.Load() || errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
			res.errCode = "timeout"
		} else {
			res.errCode = "conn"
		}
		return res
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		res.httpStatus = resp.StatusCode
		if isStream {
			return h.nativeStreamResponse(w, resp, att, attemptStart, cancel, rt, adapter, res, stopDeadline, &timedOut)
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
			res.errCode, res.status, res.retryable = "timeout", "error", true
		} else {
			res.errCode, res.status, res.retryable = "read_error", "error", true
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
	adapter ProtocolAdapter, res attemptResult, stopDeadline func(), timedOut *atomic.Bool) attemptResult {

	idle := newIdleReader(resp.Body, time.Duration(rt.StreamIdleTimeoutS)*time.Second, cancel)
	defer idle.Close()
	buf := make([]byte, 32<<10)
	flusher, _ := w.(http.Flusher)
	committed := false

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
			if _, wErr := w.Write(buf[:n]); wErr != nil {
				res.status, res.errCode = "error", "client_disconnected"
				return res
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				res.status = "success"
				return res
			}
			if timedOut.Load() {
				res.errCode, res.status = "timeout", "error"
			} else {
				res.errCode, res.status = "stream_read_error", "error"
			}
			res.streamBroke = committed
			res.retryable = !committed
			return res
		}
	}
}
