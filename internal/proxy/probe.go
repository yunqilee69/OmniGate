package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/cloudomni/omnigate/internal/config"
	"github.com/cloudomni/omnigate/internal/router"
	"github.com/cloudomni/omnigate/internal/store"
)

const (
	probeTimeoutCapMs = 15000
	probeMaxTokens    = 16
	probeBodyLimit    = 4 << 10
	probeMessageTrunc = 300
)

// ProbeResult 单模型可用性探测结果。
type ProbeResult struct {
	Model            string `json:"model"`
	ModelID          int64  `json:"model_id"`
	Provider         string `json:"provider"`
	Protocol         string `json:"protocol"`
	KeyID            int64  `json:"key_id"`
	Ok               bool   `json:"ok"`
	HTTPStatus       int    `json:"http_status"`
	LatencyMs        int64  `json:"latency_ms"`
	ErrCode          string `json:"error_code,omitempty"`
	Message          string `json:"message,omitempty"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
}

func truncateMsg(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ProbeModel 用一个极小请求真实调用上游验证模型可用性：
// 走与正式代理相同的协议适配器；不写 request_log（探测流量不计入统计）。
func ProbeModel(db *store.Store, rt *config.RuntimeManager, modelID int64) ProbeResult {
	var m store.Model
	if err := db.DB.First(&m, modelID).Error; err != nil {
		return ProbeResult{ModelID: modelID, ErrCode: "not_found", Message: "模型不存在"}
	}
	var provider store.Provider
	if err := db.DB.First(&provider, m.ProviderID).Error; err != nil {
		return ProbeResult{ModelID: m.ID, Model: m.Name, ErrCode: "no_provider", Message: "提供商不存在"}
	}
	res := ProbeResult{ModelID: m.ID, Model: m.Name, Provider: provider.Name, Protocol: m.Protocol}

	sel := router.NewSelector(db)
	snap, found, err := sel.LoadSnapshotByModel(m.ID)
	if err != nil || !found {
		res.ErrCode, res.Message = "no_key", "未绑定密钥"
		return res
	}
	att, ok := sel.PickForModel(snap, time.Now())
	if !ok {
		res.ErrCode, res.Message = "no_key", "全部密钥不可用（禁用/冷却中）"
		return res
	}
	res.KeyID = att.Key.ID

	adapter := AdapterFor(m.Protocol)
	req := map[string]any{
		"model":      m.Name,
		"max_tokens": float64(probeMaxTokens),
		"messages":   []map[string]any{{"role": "user", "content": "ping"}},
	}
	converted, err := adapter.buildBody(req)
	if err != nil {
		res.ErrCode, res.Message = "convert_error", err.Error()
		return res
	}
	body, err := marshalJSON(converted)
	if err != nil {
		res.ErrCode, res.Message = "marshal_error", err.Error()
		return res
	}

	timeoutMs := provider.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 120000
	}
	if timeoutMs > probeTimeoutCapMs {
		timeoutMs = probeTimeoutCapMs
	}
	client := &http.Client{Timeout: time.Duration(timeoutMs) * time.Millisecond}
	httpReq, err := http.NewRequest(http.MethodPost, adapter.endpoint(provider.BaseURL), bytes.NewReader(body))
	if err != nil {
		res.ErrCode, res.Message = "bad_url", err.Error()
		return res
	}
	httpReq.Header.Set("Content-Type", "application/json")
	extra := map[string]string{}
	adapter.setHeaders(extra, att.Key.KeyValue)
	for k, v := range extra {
		httpReq.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := client.Do(httpReq)
	res.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		res.ErrCode, res.Message = "conn", truncateMsg(err.Error(), probeMessageTrunc)
		return res
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, probeBodyLimit)); _ = resp.Body.Close() }()
	res.HTTPStatus = resp.StatusCode

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		res.ErrCode = strconv.Itoa(resp.StatusCode)
		res.Message = truncateMsg(string(respBody), probeMessageTrunc)
		return res
	}
	out, u, convErr := adapter.convertBuffered(respBody)
	_ = out
	if convErr == nil && (u.prompt > 0 || u.completion > 0) {
		res.PromptTokens, res.CompletionTokens = u.prompt, u.completion
	} else {
		// openai 直通适配器不解析 usage，这里按 chat 格式兜底提取
		var generic struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(respBody, &generic) == nil && generic.Usage != nil {
			res.PromptTokens, res.CompletionTokens = generic.Usage.PromptTokens, generic.Usage.CompletionTokens
		}
	}
	res.Ok = true
	return res
}

// ProbeProvider 并发探测某提供商下全部模型（并发度 4）。
func ProbeProvider(db *store.Store, rt *config.RuntimeManager, providerID int64) ([]ProbeResult, bool) {
	var provider store.Provider
	if err := db.DB.First(&provider, providerID).Error; err != nil {
		return nil, false
	}
	var models []store.Model
	if err := db.DB.Where("provider_id = ?", providerID).Order("id").Find(&models).Error; err != nil {
		return nil, false
	}
	results := make([]ProbeResult, len(models))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i := range models {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[idx] = ProbeModel(db, rt, models[idx].ID)
		}(i)
	}
	wg.Wait()
	return results, true
}

func marshalJSON(v any) ([]byte, error) { return json.Marshal(v) }
