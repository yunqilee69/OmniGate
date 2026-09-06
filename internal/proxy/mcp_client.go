package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// MCPClient Streamable HTTP MCP 客户端。
type MCPClient struct {
	targetURL string
	apiKey    string
	timeout   time.Duration
	client    *http.Client
}

// NewMCPClient 构造 MCP 客户端。
func NewMCPClient(targetURL, apiKey string, timeout time.Duration) *MCPClient {
	return &MCPClient{
		targetURL: targetURL,
		apiKey:    apiKey,
		timeout:   timeout,
		client:    &http.Client{Timeout: timeout},
	}
}

// jsonRPCRequest JSON-RPC 2.0 请求。
type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// jsonRPCResponse JSON-RPC 2.0 响应。
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// InitializeParams MCP initialize 参数。
type InitializeParams struct {
	ProtocolVersion string                 `json:"protocolVersion"`
	Capabilities    map[string]any         `json:"capabilities"`
	ClientInfo      map[string]any         `json:"clientInfo"`
	Meta            map[string]any         `json:"_meta,omitempty"`
}

// InitializeResult MCP initialize 结果。
type InitializeResult struct {
	ProtocolVersion string                 `json:"protocolVersion"`
	Capabilities    map[string]any         `json:"capabilities"`
	ServerInfo      map[string]any         `json:"serverInfo"`
	Instructions    string                 `json:"instructions,omitempty"`
	Meta            map[string]any         `json:"_meta,omitempty"`
}

// Tool MCP 工具定义。
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

// CallToolResult MCP tools/call 结果。
type CallToolResult struct {
	Content []map[string]any `json:"content"`
	IsError bool             `json:"isError,omitempty"`
	Meta    map[string]any   `json:"_meta,omitempty"`
}

// Initialize 发送 initialize 请求。
func (c *MCPClient) Initialize(ctx context.Context, params InitializeParams) (*InitializeResult, error) {
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params:  params,
	}
	var result InitializeResult
	if err := c.call(ctx, req, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ListTools 发送 tools/list 请求。
func (c *MCPClient) ListTools(ctx context.Context) ([]Tool, error) {
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/list",
		Params:  map[string]any{},
	}
	var result struct {
		Tools      []Tool `json:"tools"`
		NextCursor string `json:"nextCursor,omitempty"`
	}
	if err := c.call(ctx, req, &result); err != nil {
		return nil, err
	}
	return result.Tools, nil
}

// CallTool 发送 tools/call 请求。
func (c *MCPClient) CallTool(ctx context.Context, name string, args map[string]any) (*CallToolResult, error) {
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      3,
		Method:  "tools/call",
		Params: map[string]any{
			"name":      name,
			"arguments": args,
		},
	}
	var result CallToolResult
	if err := c.call(ctx, req, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// call 发送 JSON-RPC 请求并解析响应。
func (c *MCPClient) call(ctx context.Context, req jsonRPCRequest, result any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.targetURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("http %d: %s", resp.StatusCode, string(bodyBytes))
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	var rpcResp jsonRPCResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return fmt.Errorf("unmarshal response: %w", err)
	}

	if rpcResp.Error != nil {
		return fmt.Errorf("jsonrpc error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	if result != nil && len(rpcResp.Result) > 0 {
		if err := json.Unmarshal(rpcResp.Result, result); err != nil {
			return fmt.Errorf("unmarshal result: %w", err)
		}
	}

	return nil
}

// Close 关闭客户端（当前无需操作，保留接口供未来扩展）。
func (c *MCPClient) Close() error {
	return nil
}
