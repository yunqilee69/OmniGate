package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cloudomni/omnigate/internal/router"
)

// 非 chat 端点家族。两者均为非流式、无会话亲和（请求体里没有 messages 前缀可做指纹），
// 请求/响应按各自业界事实格式直通，仅重写 model 字段（逻辑路由名 → 物理模型名）：
//   - embeddings：OpenAI /v1/embeddings 格式，是全行业被广泛复制的的事实标准；
//   - rerank：Cohere /v1/rerank 骨架（query/documents/top_n → results[].relevance_score），
//     无官方标准，Jina/硅基流动/vLLM/TEI 均近似该形状，字段存在差异，故不做跨厂商改写。
type typedKind struct {
	modelType  string
	path       string
	parseUsage func([]byte) usageInfo
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

// Embeddings 处理 POST /v1/embeddings（OpenAI 格式）。
func (h *Handler) Embeddings(w http.ResponseWriter, r *http.Request) {
	h.serveTyped(w, r, embeddingKind)
}

// Rerank 处理 POST /v1/rerank（Cohere 骨架）。
func (h *Handler) Rerank(w http.ResponseWriter, r *http.Request) {
	h.serveTyped(w, r, rerankKind)
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
	var reqSnap string
	if captureOn {
		reqSnap = string(body)
		cw = newCaptureWriter(w, 1<<20)
		w = cw
	}

	snap, found, err := h.sel.LoadSnapshot(routeName)
	if err != nil {
		openAIError(w, 500, "internal_error", "failed to load routing config", nil)
		h.maybeCapture(requestID, routeName, reqSnap, cw)
		return
	}
	if !found {
		openAIError(w, http.StatusNotFound, "model_not_found",
			"the model '"+routeName+"' does not exist", nil)
		h.maybeCapture(requestID, routeName, reqSnap, cw)
		return
	}

	tried := map[int64]bool{}
	maxAttempts := rt.BreakerMaxHops + 1
	var last attemptResult
	var errCodes []string
	priorFails := 0

	for attempt := 0; attempt < maxAttempts; attempt++ {
		att, ok := h.sel.PickTyped(snap, tried, time.Now(), kind.modelType)
		if !ok {
			if attempt == 0 {
				statuses := router.BackendStatuses(snap, time.Now())
				h.writeLog(start, requestID, routeName, router.Attempt{}, false,
					"error", "all_backends", usageInfo{}, 0, time.Since(start), priorFails, "")
				openAIError(w, http.StatusServiceUnavailable, "all_backends_unavailable",
					"route '"+routeName+"' has no available "+kind.modelType+" type backends", statuses)
				h.maybeCapture(requestID, routeName, reqSnap, cw)
				return
			}
			break
		}
		tried[att.Key.ID] = true
		attemptStart := time.Now()
		res := h.typedAttempt(w, r, req, att, kind)
		res.latencyMs = time.Since(attemptStart).Milliseconds()
		h.record(res, rt)
		h.writeAttempt(requestID, routeName, attempt, att, res, attemptStart)
		last = res
		h.writeLog(start, requestID, routeName, att, false,
			res.status, res.errCode, res.usage, res.ttft, time.Since(start), priorFails, res.errorBody)
		if res.committed || !res.retryable {
			break
		}
		priorFails++
		errCodes = append(errCodes, res.errCode)
	}

	if !last.committed {
		openAIError(w, http.StatusBadGateway, "all_attempts_failed",
			"all attempts failed after "+strconv.Itoa(priorFails)+" retries (error sequence: "+strings.Join(errCodes, " → ")+")", nil)
		h.maybeCapture(requestID, routeName, reqSnap, cw)
		return
	}
	h.maybeCapture(requestID, routeName, reqSnap, cw)
}

// typedAttempt 单次非流式转发：出站固定 OpenAI 风格 Bearer 头 + kind.path 路径，
// 成功时响应体直通（usage 从响应按家族格式提取），失败分类与 chat attempt 一致。
func (h *Handler) typedAttempt(w http.ResponseWriter, r *http.Request, req map[string]any,
	att router.Attempt, kind typedKind) attemptResult {

	attemptStart := time.Now()
	res := attemptResult{att: att}

	req["model"] = att.Model.Name
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

	resp, err := h.client.Do(upReq)
	if err != nil {
		res.retryable, res.status = true, "error"
		if ctx.Err() == context.DeadlineExceeded {
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
		if retryableStatus[resp.StatusCode] {
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

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		res.errCode, res.status, res.retryable = "read_error", "error", true
		return res
	}
	res.usage = kind.parseUsage(respBody)
	res.committed, res.status, res.ttft = true, "success", time.Since(attemptStart)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Modelrouter-Model", att.Model.Name)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
	return res
}
