package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cloudomni/omnigate/internal/config"
	"github.com/cloudomni/omnigate/internal/router"
	"github.com/cloudomni/omnigate/internal/store"
)

// 非 chat 端点家族。均为缓冲式转发、无会话亲和（请求体里没有 messages 前缀可做指纹），
// 请求/响应按各自业界事实格式直通，仅重写 model 字段（逻辑路由名 → 物理模型名）：
//   - embeddings：OpenAI /v1/embeddings 格式，是全行业被广泛复制的的事实标准；
//   - rerank：Cohere /v1/rerank 骨架（query/documents/top_n → results[].relevance_score），
//     无官方标准，Jina/硅基流动/vLLM/TEI 均近似该形状，字段存在差异，故不做跨厂商改写；
//   - images：OpenAI Images API POST /images/generations，同为广泛复制的事实标准
//     （智谱 CogView、Azure OpenAI、硅基流动、OpenRouter Unified Image API 同形状）。
//     客户端 stream 字段原样透传：上游可能返回 SSE（gpt-image 渐进预览），网关仍为
//     缓冲式转发，响应体与 Content-Type 原样回写，流式响应的 usage 无法提取记 0。
type typedKind struct {
	modelType  string
	path       string
	parseUsage func([]byte) usageInfo
	// respLimit 上游响应体读取上限；0 用默认 32MB。images 响应内嵌 base64 图片，
	// n=10 张高分辨率图可远超 chat 响应体积，该家族放宽到 128MB。
	respLimit int64
}

var embeddingKind = typedKind{
	modelType: "embedding",
	path:      "/embeddings",
	parseUsage: func(body []byte) usageInfo {
		var parsed struct {
			Usage *struct {
				PromptTokens int `json:"prompt_tokens"`
				TotalTokens  int `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(body, &parsed) != nil || parsed.Usage == nil {
			return usageInfo{}
		}
		completion := parsed.Usage.TotalTokens - parsed.Usage.PromptTokens
		if completion < 0 {
			completion = 0
		}
		return usageInfo{prompt: parsed.Usage.PromptTokens, completion: completion}
	},
}

var rerankKind = typedKind{
	modelType: "rerank",
	path:      "/rerank",
	parseUsage: func(body []byte) usageInfo {
		var parsed struct {
			Meta *struct {
				Tokens *struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"tokens"`
				BilledUnits *struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"billed_units"`
			} `json:"meta"`
			Usage *struct {
				TotalTokens int `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(body, &parsed) != nil {
			return usageInfo{}
		}
		switch {
		case parsed.Meta != nil && parsed.Meta.Tokens != nil:
			return usageInfo{prompt: parsed.Meta.Tokens.InputTokens, completion: parsed.Meta.Tokens.OutputTokens}
		case parsed.Meta != nil && parsed.Meta.BilledUnits != nil:
			return usageInfo{prompt: parsed.Meta.BilledUnits.InputTokens, completion: parsed.Meta.BilledUnits.OutputTokens}
		case parsed.Usage != nil:
			return usageInfo{prompt: parsed.Usage.TotalTokens}
		}
		return usageInfo{}
	},
}

// imageKind 生图家族。usage 仅按 token 计费的厂商返回：gpt-image 系为
// input_tokens/output_tokens，OpenRouter 为 prompt_tokens/completion_tokens；
// 按图计费的厂商（如 CogView）无 token 用量，记 0。
var imageKind = typedKind{
	modelType: "image",
	path:      "/images/generations",
	respLimit: 128 << 20,
	parseUsage: func(body []byte) usageInfo {
		var parsed struct {
			Usage *struct {
				InputTokens      int `json:"input_tokens"`
				OutputTokens     int `json:"output_tokens"`
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(body, &parsed) != nil || parsed.Usage == nil {
			return usageInfo{}
		}
		u := parsed.Usage
		switch {
		case u.InputTokens > 0 || u.OutputTokens > 0:
			return usageInfo{prompt: u.InputTokens, completion: u.OutputTokens}
		case u.PromptTokens > 0 || u.CompletionTokens > 0:
			return usageInfo{prompt: u.PromptTokens, completion: u.CompletionTokens}
		case u.TotalTokens > 0:
			return usageInfo{prompt: u.TotalTokens}
		}
		return usageInfo{}
	},
}

// Embeddings 处理 POST /v1/embeddings（OpenAI 格式）。
func (h *Handler) Embeddings(w http.ResponseWriter, r *http.Request) {
	h.serveTyped(w, r, embeddingKind)
}

// Rerank 处理 POST /v1/rerank（Cohere 骨架）。
func (h *Handler) Rerank(w http.ResponseWriter, r *http.Request) {
	h.serveTyped(w, r, rerankKind)
}

// Images 处理 POST /v1/images/generations（OpenAI Images 格式）。
func (h *Handler) Images(w http.ResponseWriter, r *http.Request) {
	h.serveTyped(w, r, imageKind)
}

func (h *Handler) serveTyped(w http.ResponseWriter, r *http.Request, kind typedKind) {
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
		openAIError(w, 500, "internal_error", "failed to load routing config", nil)
		h.maybeCapture(requestID, routeName, cw)
		return
	}
	if !found {
		openAIError(w, http.StatusNotFound, "model_not_found",
			"the model '"+routeName+"' does not exist", nil)
		h.maybeCapture(requestID, routeName, cw)
		return
	}

	var vkID int64
	if vk, ok := getVKFromContext(r.Context()); ok {
		vkID = vk.ID
		if err := checkVKRouteAccess(h.db, vk, snap.Route.ID); err != nil {
			if err == store.ErrVKAccessDenied {
				openAIError(w, 403, "route_denied", "route '"+routeName+"' not allowed by virtual key", nil)
			} else {
				openAIError(w, 500, "route_check_error", err.Error(), nil)
			}
			h.maybeCapture(requestID, routeName, cw)
			return
		}
	}

	pendingID := h.createPendingLog(requestID, routeName, kind.modelType, false, vkID)
	tried := map[router.Combo]bool{}
	maxAttempts := rt.BreakerMaxHops + 1
	var last attemptResult
	var errCodes []string
	priorFails := 0
	var attempts []store.RequestAttempt

	for attempt := 0; attempt < maxAttempts; attempt++ {
		att, ok := h.sel.PickTyped(snap, tried, time.Now(), kind.modelType)
		if !ok {
			if attempt == 0 {
				if rt.FallbackEnabled && rt.FallbackModelID > 0 {
					fallbackAtt, fallbackOK := h.sel.PickFallback(rt.FallbackModelID, time.Now())
					if fallbackOK {
						slog.Info("using fallback model", "route", routeName, "fallback_model_id", rt.FallbackModelID, "type", kind.modelType)
						attemptStart := time.Now()
						res := h.typedAttempt(w, r, req, fallbackAtt, kind, rt)
						res.latencyMs = time.Since(attemptStart).Milliseconds()
						h.record(res, rt)
						attempts = append(attempts, h.attemptRow(requestID, routeName, 0, fallbackAtt, res, attemptStart))
						h.writeLog(start, requestID, routeName, fallbackAtt, false,
							res.status, res.errCode, res.usage, res.ttft, time.Since(start), 0, res.errorBody, true, vkID, pendingID, attempts)
						cw.setAttempt(res)
						h.maybeCapture(requestID, routeName, cw)
						return
					}
					slog.Warn("fallback model unavailable", "route", routeName, "fallback_model_id", rt.FallbackModelID, "type", kind.modelType)
				}

				// all_backends 错误：没有可用模型，仍需记录尝试
				attempts = append(attempts, h.attemptRow(requestID, routeName, 0, router.Attempt{}, attemptResult{
					status:  "error",
					errCode: "all_backends",
				}, start))
				statuses := h.sel.BackendStatuses(snap, time.Now())
				h.writeLog(start, requestID, routeName, router.Attempt{}, false,
					"error", "all_backends", usageInfo{}, 0, time.Since(start), priorFails, "", false, vkID, pendingID, attempts)
				openAIError(w, http.StatusServiceUnavailable, "all_backends_unavailable",
					"route '"+routeName+"' has no available "+kind.modelType+" type backends", statuses)
				h.maybeCapture(requestID, routeName, cw)
				return
			}
			break
		}
		tried[att.Combo()] = true
		attemptStart := time.Now()
		res := h.typedAttempt(w, r, req, att, kind, rt)
		res.latencyMs = time.Since(attemptStart).Milliseconds()
		h.record(res, rt)
		attempts = append(attempts, h.attemptRow(requestID, routeName, attempt, att, res, attemptStart))
		last = res
		if res.committed || !res.retryable {
			h.writeLog(start, requestID, routeName, att, false,
				res.status, res.errCode, res.usage, res.ttft, time.Since(start), priorFails, res.errorBody, false, vkID, pendingID, attempts)
			break
		}
		priorFails++
		errCodes = append(errCodes, res.errCode)
	}

	// 重试耗尽 / 转移途中无后端可选：最终结果尚未落库时在此补写。
	if last.att.Model.ID != 0 && !last.committed && last.retryable {
		h.writeLog(start, requestID, routeName, last.att, false,
			last.status, last.errCode, last.usage, last.ttft, time.Since(start), priorFails-1, last.errorBody, false, vkID, pendingID, attempts)
	}

	if !last.committed {
		openAIError(w, http.StatusBadGateway, "all_attempts_failed",
			"all attempts failed after "+strconv.Itoa(priorFails)+" retries (error sequence: "+strings.Join(errCodes, " → ")+")", nil)
		h.maybeCapture(requestID, routeName, cw)
		return
	}
	if last.att.Model.ID != 0 {
		cw.setAttempt(last)
	}
	h.maybeCapture(requestID, routeName, cw)
}

// typedAttempt 单次转发：出站固定 OpenAI 风格 Bearer 头 + kind.path 路径，
// 成功时响应体与 Content-Type 直通（usage 从响应按家族格式提取），失败分类与 chat attempt 一致。
func (h *Handler) typedAttempt(w http.ResponseWriter, r *http.Request, req map[string]any,
	att router.Attempt, kind typedKind, rt *config.Runtime) attemptResult {

	attemptStart := time.Now()
	res := attemptResult{att: att}

	req["model"] = att.Model.Name
	// 应用模型级 body_override：与 chat 路径同语义（覆盖优先于客户端字段），
	// 生图 size/quality 等厂商参数预设由此下发
	if att.Model.BodyOverride != "" {
		var override map[string]any
		if err := json.Unmarshal([]byte(att.Model.BodyOverride), &override); err == nil {
			for k, v := range override {
				req[k] = v
			}
		}
	}
	outBody, err := json.Marshal(req)
	if err != nil {
		res.errCode, res.status = "marshal_error", "error"
		return res
	}

	timeoutMs := att.Provider.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 120000
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(att.Provider.BaseURL, "/")+kind.path, bytes.NewReader(outBody))
	if err != nil {
		res.errCode, res.status = "bad_upstream_url", "error"
		return res
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "Bearer "+att.Key.KeyValue)
	ApplyUpstreamIdentity(upReq, att.Provider, r)
	// 出站快照：内容捕获（content_log）记录的是 OmniGate → 上游的实际请求（头已含模拟/认证头）
	res.reqHeaders = formatHeaders(upReq.Header)
	res.reqBody = outBody

	resp, err := h.clientForProvider(att.Provider.ID).Do(upReq)
	if err != nil {
		res.retryable, res.status = true, "error"
		if ctx.Err() == context.DeadlineExceeded {
			res.errCode = "timeout"
		} else {
			res.errCode = "conn"
		}
		return res
	}
	res.respHeaders = resp.Header
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	res.httpStatus = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
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

	limit := kind.respLimit
	if limit <= 0 {
		limit = maxBodyBytes
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		res.errCode, res.status, res.retryable = "read_error", "error", true
		return res
	}
	res.usage = kind.parseUsage(respBody)
	res.committed, res.status, res.ttft = true, "success", time.Since(attemptStart)
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Modelrouter-Model", att.Model.Name)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
	return res
}
