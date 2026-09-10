package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
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

// KeyProbeResult 逐密钥探测结果：附密钥标识、当前状态与模型-密钥组合禁用状态，供管理台测试弹窗展示与操作。
type KeyProbeResult struct {
	ProbeResult
	KeyName   string `json:"key_name"`
	KeyMasked string `json:"key_masked"`
	KeyStatus string `json:"key_status"`
	Banned    bool   `json:"banned"`               // 该模型下的此密钥组合是否被禁用（手动或失败归因）
	BanReason string `json:"ban_reason,omitempty"` // 组合禁用原因（banned=true 时）
}

// ModelKeysTestResult 单模型全量密钥探测结果。
type ModelKeysTestResult struct {
	ModelID  int64            `json:"model_id"`
	Model    string           `json:"model"`
	Provider string           `json:"provider"`
	Protocol string           `json:"protocol"`
	Keys     []KeyProbeResult `json:"keys"`
}

func maskKeyValue(kv string) string {
	if len(kv) <= 8 {
		return "****"
	}
	return kv[:5] + "****" + kv[len(kv)-4:]
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

	sel := router.NewSelector(db)
	snap, found, err := sel.LoadSnapshotByModel(m.ID)
	if err != nil || !found {
		return ProbeResult{ModelID: m.ID, Model: m.Name, Provider: provider.Name, Protocol: m.Protocol,
			ErrCode: "no_key", Message: "未绑定密钥"}
	}
	att, ok := sel.PickForModel(snap, time.Now())
	if !ok {
		return ProbeResult{ModelID: m.ID, Model: m.Name, Provider: provider.Name, Protocol: m.Protocol,
			ErrCode: "no_key", Message: "全部密钥不可用（禁用/冷却中）"}
	}
	return probeModelKey(m, provider, att.Key)
}

// ProbeModelKeys 并发探测模型绑定的每一个密钥（含禁用/冷却中的——手动诊断动作，非代理流量）。
func ProbeModelKeys(db *store.Store, rt *config.RuntimeManager, modelID int64) (ModelKeysTestResult, bool) {
	var m store.Model
	if err := db.DB.First(&m, modelID).Error; err != nil {
		return ModelKeysTestResult{ModelID: modelID, Keys: []KeyProbeResult{}}, false
	}
	out := ModelKeysTestResult{ModelID: m.ID, Model: m.Name, Protocol: m.Protocol, Keys: []KeyProbeResult{}}
	var provider store.Provider
	if err := db.DB.First(&provider, m.ProviderID).Error; err == nil {
		out.Provider = provider.Name
	}

	var keys []store.ApiKey
	if err := db.DB.
		Joins("JOIN model_key mk ON mk.key_id = api_key.id").
		Where("mk.model_id = ?", modelID).
		Order("api_key.id").Find(&keys).Error; err != nil {
		return out, true
	}

	// 组合禁用状态：一次查询本模型全部 ban，避免逐 key 查询
	banByKey := map[int64]store.ModelKeyBan{}
	var bans []store.ModelKeyBan
	if err := db.DB.Where("model_id = ?", modelID).Find(&bans).Error; err == nil {
		for _, b := range bans {
			banByKey[b.KeyID] = b
		}
	}
	now := time.Now().Unix()

	results := make([]KeyProbeResult, len(keys))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			k := keys[idx]
			r := KeyProbeResult{
				ProbeResult: probeModelKey(m, provider, k),
				KeyName:     k.Name,
				KeyMasked:   maskKeyValue(k.KeyValue),
				KeyStatus:   k.Status,
			}
			if b, ok := banByKey[k.ID]; ok {
				if b.Status == "perm_banned" || (b.Status == "temp_banned" && b.BannedUntil > now) {
					r.Banned = true
					r.BanReason = b.BanReason
				}
			}
			results[idx] = r
		}(i)
	}
	wg.Wait()
	out.Keys = results
	return out, true
}

// probeModelKey 用指定密钥发起一次极小真实请求（探测核心，不写 request_log）。
// 按模型类型构造最小载荷：chat → 一条 ping 消息；embedding → 单串输入；rerank → 单文档重排；image → 一句生图提示。
func probeModelKey(m store.Model, provider store.Provider, key store.ApiKey) ProbeResult {
	res := ProbeResult{ModelID: m.ID, Model: m.Name, Provider: provider.Name, Protocol: m.Protocol, KeyID: key.ID}

	modelType := m.Type
	if modelType == "" {
		modelType = "chat"
	}
	adapter := AdapterFor(m.Protocol)
	var req map[string]any
	var endpoint string
	switch modelType {
	case "embedding":
		req = map[string]any{"model": m.Name, "input": "ping"}
		endpoint = strings.TrimRight(provider.BaseURL, "/") + "/embeddings"
	case "rerank":
		req = map[string]any{"model": m.Name, "query": "ping", "documents": []string{"pong"}}
		endpoint = strings.TrimRight(provider.BaseURL, "/") + "/rerank"
	case "image":
		req = map[string]any{"model": m.Name, "prompt": "ping"}
		endpoint = strings.TrimRight(provider.BaseURL, "/") + "/images/generations"
	default:
		req = map[string]any{
			"model":      m.Name,
			"max_tokens": float64(probeMaxTokens),
			"messages":   []map[string]any{{"role": "user", "content": "ping"}},
		}
		endpoint = adapter.endpoint(provider.BaseURL, &m)
	}
	converted, err := adapter.buildBody(req)
	if err != nil {
		res.ErrCode, res.Message = "convert_error", err.Error()
		return res
	}
	// 应用 body_override：合并覆盖字段到转换后的请求体
	if m.BodyOverride != "" {
		var override map[string]any
		if err := json.Unmarshal([]byte(m.BodyOverride), &override); err == nil {
			for k, v := range override {
				converted[k] = v
			}
		}
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
	httpReq, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		res.ErrCode, res.Message = "bad_url", err.Error()
		return res
	}
	httpReq.Header.Set("Content-Type", "application/json")
	extra := map[string]string{}
	adapter.setHeaders(extra, key.KeyValue)
	for k, v := range extra {
		httpReq.Header.Set(k, v)
	}
	ApplyUpstreamIdentity(httpReq, provider, nil)

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
	} else if modelType == "embedding" {
		e := embeddingKind.parseUsage(respBody)
		res.PromptTokens, res.CompletionTokens = e.prompt, e.completion
	} else if modelType == "rerank" {
		e := rerankKind.parseUsage(respBody)
		res.PromptTokens, res.CompletionTokens = e.prompt, e.completion
	} else if modelType == "image" {
		e := imageKind.parseUsage(respBody)
		res.PromptTokens, res.CompletionTokens = e.prompt, e.completion
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
