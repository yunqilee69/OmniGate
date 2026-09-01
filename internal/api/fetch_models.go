package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// fetchModelItem 提供商 /v1/models 响应中单个模型的结构。
type fetchModelItem struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type fetchModelsReq struct {
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"`
	ProxyURL string `json:"proxy_url"`
}

type fetchModelsResp struct {
	Models []fetchModelItem `json:"models"`
}

// fetchProviderModels 代理调用提供商的 /v1/models 接口，返回可用模型列表。
// 请求体包含 base_url 和 api_key，支持可选的 proxy_url。
func (s *Server) fetchProviderModels(w http.ResponseWriter, r *http.Request) {
	var req fetchModelsReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	req.BaseURL = strings.TrimSpace(req.BaseURL)
	req.APIKey = strings.TrimSpace(req.APIKey)
	req.ProxyURL = strings.TrimSpace(req.ProxyURL)

	if req.BaseURL == "" || req.APIKey == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "base_url and api_key are required")
		return
	}

	models, err := callProviderModels(req.BaseURL, req.APIKey, req.ProxyURL)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "fetch_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, fetchModelsResp{Models: models})
}

// callProviderModels 向提供商发起 GET {baseURL}/v1/models 请求并解析返回的模型列表。
func callProviderModels(baseURL, apiKey, proxyURL string) ([]fetchModelItem, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	
	// 智能拼接路径，避免重复 /v1
	endpoint := baseURL
	if strings.HasSuffix(baseURL, "/v1") {
		endpoint = baseURL + "/models"
	} else {
		endpoint = baseURL + "/v1/models"
	}

	client := &http.Client{Timeout: 30 * time.Second}
	if proxyURL != "" {
		proxy, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy_url: %w", err)
		}
		client.Transport = &http.Transport{
			Proxy: http.ProxyURL(proxy),
		}
	}

	httpReq, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "OmniGate/1.0")

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		preview := string(body)
		if len(preview) > 500 {
			preview = preview[:500]
		}
		return nil, fmt.Errorf("provider returned %d: %s", resp.StatusCode, preview)
	}

	var result struct {
		Data []fetchModelItem `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return result.Data, nil
}