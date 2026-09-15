package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudomni/omnigate/internal/config"
	"github.com/cloudomni/omnigate/internal/router"
	"github.com/cloudomni/omnigate/internal/store"
)

// 音频端点家族。TTS 入站 JSON、出站二进制/SSE 流式透传；STT 入站 multipart 全缓冲、出站缓冲回写。
// 不做跨厂商协议转换，仅重写 model 字段 + 合并 body_override；与 embeddings/rerank/images 同方针。
type audioKind struct {
	modelType string // "tts" | "stt"，同时写入 request_log.endpoint
	path      string
	isSTT     bool
}

var ttsKind = audioKind{modelType: "tts", path: "/audio/speech"}
var sttKind = audioKind{modelType: "stt", path: "/audio/transcriptions", isSTT: true}

// Speech 处理 POST /v1/audio/speech（OpenAI TTS）。
func (h *Handler) Speech(w http.ResponseWriter, r *http.Request) {
	h.serveAudio(w, r, ttsKind)
}

// Transcriptions 处理 POST /v1/audio/transcriptions（OpenAI STT）。
func (h *Handler) Transcriptions(w http.ResponseWriter, r *http.Request) {
	h.serveAudio(w, r, sttKind)
}

type audioReq struct {
	routeName string
	// TTS
	jsonBody map[string]any
	chars    int
	// STT
	parts     []mpPart
	fileName  string
	fileBytes []byte
}

type mpPart struct {
	Header textproto.MIMEHeader
	Bytes  []byte
	name   string
}

func (h *Handler) serveAudio(w http.ResponseWriter, r *http.Request, kind audioKind) {
	start := time.Now()
	requestID := newRequestID()
	w.Header().Set("X-Request-Id", requestID)

	rt := h.rt.Snapshot()
	areq, errCode, errMsg, httpStatus := parseAudioRequest(r, kind, rt)
	if errCode != "" {
		openAIError(w, httpStatus, errCode, errMsg, nil)
		return
	}
	routeName := areq.routeName

	captureOn := rt.CaptureEnabled && (len(rt.CaptureRoutes) == 0 || containsStr(rt.CaptureRoutes, routeName))
	var cw *captureWriter
	if captureOn {
		cw = newCaptureWriter(w, 1<<20)
		w = cw
		cw.setClientReq(r.Header, audioClientCapture(areq, kind))
		if !kind.isSTT {
			cw.skipResponseBody("[binary audio response]")
		}
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

	isStream := !kind.isSTT // TTS 出站按流处理（二进制也走流式 copy）
	pendingID := h.createPendingLog(requestID, routeName, kind.modelType, isStream, vkID)
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
			slog.Warn("fallback model unavailable", "route", routeName, "fallback_model_id", fbID, "type", kind.modelType)
			return false
		}
		slog.Info("using fallback model", "route", routeName, "fallback_model_id", fbID, "type", kind.modelType)
		attemptStart := time.Now()
		res := h.audioAttempt(w, r, areq, fallbackAtt, kind, rt)
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
		att, ok := h.sel.PickTyped(snap, tried, time.Now(), kind.modelType)
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
					"route '"+routeName+"' has no available "+kind.modelType+" type backends", statuses)
				h.maybeCapture(requestID, routeName, cw)
				return
			}
			break
		}
		tried[att.Combo()] = true
		attemptStart := time.Now()
		res := h.audioAttempt(w, r, areq, att, kind, rt)
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
			"all attempts failed after "+strconv.Itoa(priorFails)+" retries (error sequence: "+strings.Join(errCodes, " → ")+")", nil)
		h.maybeCapture(requestID, routeName, cw)
		return
	}
	if last.att.Model.ID != 0 {
		cw.setAttempt(last)
	}
	if !kind.isSTT && cw != nil {
		ct := last.respHeaders.Get("Content-Type")
		cw.skipResponseBody(fmt.Sprintf("[binary audio response, content-type: %s]", ct))
	}
	h.maybeCapture(requestID, routeName, cw)
}

func parseAudioRequest(r *http.Request, kind audioKind, rt *config.Runtime) (audioReq, string, string, int) {
	if kind.isSTT {
		return parseSTTRequest(r, rt)
	}
	return parseTTSRequest(r)
}

func parseTTSRequest(r *http.Request) (audioReq, string, string, int) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return audioReq{}, errReadFailed, "failed to read request body", 400
	}
	if len(body) > maxBodyBytes {
		return audioReq{}, "request_too_large", "request body exceeds 32MB", http.StatusRequestEntityTooLarge
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return audioReq{}, "invalid_json", "request body is not valid JSON", 400
	}
	routeName, _ := req["model"].(string)
	if routeName == "" {
		return audioReq{}, "missing_model", "request body must contain a model field", 400
	}
	input, _ := req["input"].(string)
	if input == "" {
		return audioReq{}, "missing_input", "request body must contain an input field", 400
	}
	return audioReq{routeName: routeName, jsonBody: req, chars: utf8.RuneCountInString(input)}, "", "", 0
}

func parseSTTRequest(r *http.Request, rt *config.Runtime) (audioReq, string, string, int) {
	ct := r.Header.Get("Content-Type")
	mediatype, params, err := mime.ParseMediaType(ct)
	if err != nil || !strings.HasPrefix(mediatype, "multipart/") {
		return audioReq{}, "invalid_content_type", "request must be multipart/form-data", 400
	}
	boundary := params["boundary"]
	if boundary == "" {
		return audioReq{}, "invalid_content_type", "multipart boundary is required", 400
	}
	limitMB := rt.MaxAudioUploadMB
	if limitMB <= 0 {
		limitMB = 25
	}
	limit := int64(limitMB) << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return audioReq{}, errReadFailed, "failed to read request body", 400
	}
	if int64(len(body)) > limit {
		return audioReq{}, errAudioTooLarge, fmt.Sprintf("audio upload exceeds %dMB", limitMB), http.StatusRequestEntityTooLarge
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	var parts []mpPart
	var routeName, fileName string
	var fileBytes []byte
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return audioReq{}, "invalid_multipart", "failed to parse multipart body", 400
		}
		b, err := io.ReadAll(p)
		_ = p.Close()
		if err != nil {
			return audioReq{}, errReadFailed, "failed to read multipart part", 400
		}
		name := p.FormName()
		part := mpPart{Header: p.Header, Bytes: b, name: name}
		parts = append(parts, part)
		if name == "model" {
			routeName = strings.TrimSpace(string(b))
		}
		if name == "file" {
			fileName = p.FileName()
			fileBytes = b
		}
	}
	if routeName == "" {
		return audioReq{}, "missing_model", "multipart body must contain a model field", 400
	}
	if len(fileBytes) == 0 {
		return audioReq{}, "missing_file", "multipart body must contain a file field", 400
	}
	return audioReq{routeName: routeName, parts: parts, fileName: fileName, fileBytes: fileBytes}, "", "", 0
}

func (h *Handler) audioAttempt(w http.ResponseWriter, r *http.Request, areq audioReq,
	att router.Attempt, kind audioKind, rt *config.Runtime) attemptResult {
	if kind.isSTT {
		return h.sttAttempt(w, r, areq, att, kind, rt)
	}
	return h.ttsAttempt(w, r, areq, att, kind, rt)
}

func audioUpstreamURL(att router.Attempt, kind audioKind) string {
	if att.Model.ApiPath != "" {
		return att.Model.ApiPath
	}
	return strings.TrimRight(att.Provider.BaseURL, "/") + kind.path
}

func applyJSONOverride(req map[string]any, raw string) {
	if raw == "" {
		return
	}
	var override map[string]any
	if err := json.Unmarshal([]byte(raw), &override); err != nil {
		return
	}
	for k, v := range override {
		req[k] = v
	}
}

func (h *Handler) ttsAttempt(w http.ResponseWriter, r *http.Request, areq audioReq,
	att router.Attempt, kind audioKind, rt *config.Runtime) attemptResult {
	attemptStart := time.Now()
	res := attemptResult{att: att}

	req := cloneMap(areq.jsonBody)
	req["model"] = att.Model.Name
	applyJSONOverride(req, att.Model.BodyOverride)
	outBody, err := json.Marshal(req)
	if err != nil {
		res.errCode, res.status = errMarshalFailed, "error"
		return res
	}
	res.usage.chars = areq.chars

	timeoutMs := att.Provider.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 120000
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, audioUpstreamURL(att, kind), bytes.NewReader(outBody))
	if err != nil {
		res.errCode, res.status = errBadUpstreamURL, "error"
		return res
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "application/octet-stream")
	upReq.Header.Set("Authorization", "Bearer "+att.Key.KeyValue)
	ApplyUpstreamIdentity(upReq, att.Provider, r)
	res.reqURL = captureURL(upReq.URL)
	res.reqHeaders = formatHeaders(upReq.Header)
	res.reqBody = outBody

	resp, err := h.clientForProvider(att.Provider.ID).Do(upReq)
	if err != nil {
		res.retryable, res.status = true, "error"
		if ctx.Err() == context.DeadlineExceeded {
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
	res.httpStatus = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return classifyHTTPError(w, resp, &res, rt)
	}

	ct := resp.Header.Get("Content-Type")
	media, _, _ := mime.ParseMediaType(ct)
	switch {
	case media == "text/event-stream" || strings.HasPrefix(ct, "text/event-stream"):
		return streamTTS(w, resp, att, attemptStart, res)
	case strings.HasPrefix(media, "audio/") || media == "application/octet-stream" || media == "":
		return copyAudioBinary(w, resp, att, attemptStart, res, ct)
	default:
		// 兼容端点用 200 + JSON 返业务错误
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		if err != nil {
			res.errCode, res.status, res.retryable = errReadFailed, "error", true
			return res
		}
		if bytes.Contains(body, []byte(`"error"`)) {
			res.committed, res.status = true, "client_error"
			res.errCode = "upstream_error"
			res.errorBody = captureErrBody(body)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write(body)
			return res
		}
		res.committed, res.status, res.ttft = true, "success", time.Since(attemptStart)
		if ct == "" {
			ct = "application/json; charset=utf-8"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("X-Modelrouter-Model", att.Model.Name)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		return res
	}
}

func classifyHTTPError(w http.ResponseWriter, resp *http.Response, res *attemptResult, rt *config.Runtime) attemptResult {
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
		return *res
	}
	res.committed, res.status = true, "client_error"
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(errBody)
	return *res
}

func copyAudioBinary(w http.ResponseWriter, resp *http.Response, att router.Attempt, start time.Time, res attemptResult, ct string) attemptResult {
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Modelrouter-Model", att.Model.Name)
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	w.WriteHeader(resp.StatusCode)
	res.committed, res.status, res.ttft = true, "success", time.Since(start)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, wErr := w.Write(buf[:n]); wErr != nil {
				res.status, res.errCode, res.streamBroke = "error", errClientDisconnected, true
				return res
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				res.status, res.errCode, res.streamBroke = "error", errStreamBroken, true
			}
			return res
		}
	}
}

func streamTTS(w http.ResponseWriter, resp *http.Response, att router.Attempt, start time.Time, res attemptResult) attemptResult {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Modelrouter-Model", att.Model.Name)
	w.WriteHeader(http.StatusOK)
	res.committed, res.status, res.ttft = true, "success", time.Since(start)
	flusher, _ := w.(http.Flusher)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var eventBuf []byte
	flushEvent := func() {
		if len(eventBuf) == 0 {
			return
		}
		if bytes.Contains(eventBuf, []byte(`"speech.audio.done"`)) {
			if u, ok := parseSpeechDoneUsage(eventBuf); ok {
				res.usage.prompt = u.prompt
				res.usage.completion = u.completion
			}
		}
		eventBuf = eventBuf[:0]
	}
	for sc.Scan() {
		line := sc.Bytes()
		out := append(append([]byte(nil), line...), '\n')
		if _, wErr := w.Write(out); wErr != nil {
			res.status, res.errCode, res.streamBroke = "error", errClientDisconnected, true
			return res
		}
		if flusher != nil {
			flusher.Flush()
		}
		if len(line) == 0 {
			flushEvent()
			continue
		}
		eventBuf = append(eventBuf, line...)
		eventBuf = append(eventBuf, '\n')
	}
	flushEvent()
	if err := sc.Err(); err != nil {
		res.status, res.errCode, res.streamBroke = "error", errStreamBroken, true
	}
	return res
}

func parseSpeechDoneUsage(frame []byte) (usageInfo, bool) {
	idx := bytes.Index(frame, []byte("data:"))
	if idx < 0 {
		return usageInfo{}, false
	}
	payload := bytes.TrimSpace(frame[idx+5:])
	var parsed struct {
		Type  string `json:"type"`
		Usage *struct {
			InputTokens      int `json:"input_tokens"`
			OutputTokens     int `json:"output_tokens"`
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &parsed) != nil || parsed.Usage == nil {
		return usageInfo{}, false
	}
	u := parsed.Usage
	if u.InputTokens > 0 || u.OutputTokens > 0 {
		return usageInfo{prompt: u.InputTokens, completion: u.OutputTokens}, true
	}
	if u.PromptTokens > 0 || u.CompletionTokens > 0 {
		return usageInfo{prompt: u.PromptTokens, completion: u.CompletionTokens}, true
	}
	return usageInfo{}, false
}

func (h *Handler) sttAttempt(w http.ResponseWriter, r *http.Request, areq audioReq,
	att router.Attempt, kind audioKind, rt *config.Runtime) attemptResult {
	attemptStart := time.Now()
	res := attemptResult{att: att}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, p := range areq.parts {
		h := textproto.MIMEHeader{}
		for k, vs := range p.Header {
			h[k] = append([]string(nil), vs...)
		}
		pw, err := mw.CreatePart(h)
		if err != nil {
			res.errCode, res.status = errMarshalFailed, "error"
			return res
		}
		payload := p.Bytes
		if p.name == "model" {
			payload = []byte(att.Model.Name)
		}
		if _, err := pw.Write(payload); err != nil {
			res.errCode, res.status = errMarshalFailed, "error"
			return res
		}
	}
	if err := mw.Close(); err != nil {
		res.errCode, res.status = errMarshalFailed, "error"
		return res
	}
	outBody := body.Bytes()

	timeoutMs := att.Provider.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 120000
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, audioUpstreamURL(att, kind), bytes.NewReader(outBody))
	if err != nil {
		res.errCode, res.status = errBadUpstreamURL, "error"
		return res
	}
	upReq.Header.Set("Content-Type", mw.FormDataContentType())
	upReq.Header.Set("Authorization", "Bearer "+att.Key.KeyValue)
	ApplyUpstreamIdentity(upReq, att.Provider, r)
	res.reqURL = captureURL(upReq.URL)
	res.reqHeaders = formatHeaders(upReq.Header)
	res.reqBody = []byte(sttCaptureBody(areq, att.Model.Name))

	resp, err := h.clientForProvider(att.Provider.ID).Do(upReq)
	if err != nil {
		res.retryable, res.status = true, "error"
		if ctx.Err() == context.DeadlineExceeded {
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
	res.httpStatus = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return classifyHTTPError(w, resp, &res, rt)
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		res.errCode, res.status, res.retryable = errReadFailed, "error", true
		return res
	}
	res.usage = parseSTTUsage(respBody, areq)
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

func parseSTTUsage(body []byte, areq audioReq) usageInfo {
	var parsed struct {
		Duration float64 `json:"duration"`
		Usage    *struct {
			Type             string  `json:"type"`
			Seconds          float64 `json:"seconds"`
			InputTokens      int     `json:"input_tokens"`
			OutputTokens     int     `json:"output_tokens"`
			PromptTokens     int     `json:"prompt_tokens"`
			CompletionTokens int     `json:"completion_tokens"`
		} `json:"usage"`
	}
	u := usageInfo{}
	if json.Unmarshal(body, &parsed) == nil {
		if parsed.Usage != nil {
			if parsed.Usage.Seconds > 0 {
				u.seconds = parsed.Usage.Seconds
			}
			if parsed.Usage.InputTokens > 0 || parsed.Usage.OutputTokens > 0 {
				u.prompt, u.completion = parsed.Usage.InputTokens, parsed.Usage.OutputTokens
			} else if parsed.Usage.PromptTokens > 0 || parsed.Usage.CompletionTokens > 0 {
				u.prompt, u.completion = parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens
			}
		}
		if u.seconds <= 0 && parsed.Duration > 0 {
			u.seconds = parsed.Duration
		}
	}
	if u.seconds <= 0 {
		if sec, ok := audioDuration(areq.fileName, areq.fileBytes); ok {
			u.seconds = sec
		}
	}
	return u
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func audioClientCapture(areq audioReq, kind audioKind) []byte {
	if !kind.isSTT {
		b, _ := json.Marshal(areq.jsonBody)
		return b
	}
	return []byte(sttCaptureBody(areq, areq.routeName))
}

func sttCaptureBody(areq audioReq, model string) string {
	var b strings.Builder
	b.WriteString("multipart fields:\n")
	for _, p := range areq.parts {
		if p.name == "file" {
			fmt.Fprintf(&b, "file: [audio file: %s, %d bytes]\n", areq.fileName, len(areq.fileBytes))
			continue
		}
		val := string(p.Bytes)
		if p.name == "model" {
			val = model
		}
		fmt.Fprintf(&b, "%s: %s\n", p.name, val)
	}
	return b.String()
}
