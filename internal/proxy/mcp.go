package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cloudomni/omnigate/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// mcpSession MCP 会话：客户端会话 → 多个后端子会话。
type mcpSession struct {
	id        string
	backends  map[int64]*MCPClient // backend_id → client
	toolCache map[string]int64     // tool_name → backend_id (用于 tools/call 路由)
	createdAt time.Time
	mu        sync.RWMutex
}

// mcpSessionStore 全局会话存储。
type mcpSessionStore struct {
	sessions map[string]*mcpSession
	mu       sync.RWMutex
}

var globalMcpSessions = &mcpSessionStore{
	sessions: make(map[string]*mcpSession),
}

func (s *mcpSessionStore) get(id string) (*mcpSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[id]
	return sess, ok
}

func (s *mcpSessionStore) set(id string, sess *mcpSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = sess
}

func (s *mcpSessionStore) delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		sess.mu.Lock()
		for _, client := range sess.backends {
			_ = client.Close()
		}
		sess.mu.Unlock()
		delete(s.sessions, id)
	}
}

// Mcp 处理 /v1/mcp/{route_name} 请求。
func (h *Handler) Mcp(w http.ResponseWriter, r *http.Request) {
	routeName := chi.URLParam(r, "route_name")
	if routeName == "" {
		openAIError(w, http.StatusBadRequest, "invalid_request", "route_name is required", nil)
		return
	}

	// 虚拟 key 鉴权
	vk, ok := getVKFromContext(r.Context())
	if !ok {
		openAIError(w, http.StatusUnauthorized, "unauthorized", "virtual key required", nil)
		return
	}

	// 加载路由快照
	snap, found, err := h.sel.LoadSnapshot(routeName)
	if err != nil {
		slog.Error("mcp: load snapshot failed", "route", routeName, "err", err)
		openAIError(w, http.StatusInternalServerError, "internal_error", "failed to load route", nil)
		return
	}
	if !found {
		openAIError(w, http.StatusNotFound, "not_found", "route not found", nil)
		return
	}

	// 检查路由是否为 mcp 端点
	if snap.Route.Endpoint != "mcp" {
		openAIError(w, http.StatusBadRequest, "invalid_endpoint", "route is not an mcp endpoint", nil)
		return
	}

	// 检查虚拟 key 是否有权访问该路由
	if err := checkVKRouteAccess(h.db, vk, snap.Route.ID); err != nil {
		openAIError(w, http.StatusForbidden, "access_denied", err.Error(), nil)
		return
	}

	// 加载 MCP 后端列表
	var mcpTargets []store.RouteMcpTarget
	if err := h.db.DB.Where("route_id = ?", snap.Route.ID).Find(&mcpTargets).Error; err != nil {
		slog.Error("mcp: load mcp targets failed", "route", routeName, "err", err)
		openAIError(w, http.StatusInternalServerError, "internal_error", "failed to load mcp targets", nil)
		return
	}
	if len(mcpTargets) == 0 {
		openAIError(w, http.StatusBadRequest, "invalid_route", "route has no mcp backends", nil)
		return
	}

	var backendIDs []int64
	for _, t := range mcpTargets {
		backendIDs = append(backendIDs, t.MCPBackendID)
	}

	var backends []store.MCPBackend
	if err := h.db.DB.Where("id IN ?", backendIDs).Find(&backends).Error; err != nil {
		slog.Error("mcp: load backends failed", "route", routeName, "err", err)
		openAIError(w, http.StatusInternalServerError, "internal_error", "failed to load backends", nil)
		return
	}

	// 根据请求方法分发
	switch r.Method {
	case http.MethodPost:
		h.mcpPost(w, r, routeName, backends)
	case http.MethodGet:
		h.mcpGet(w, r, routeName)
	case http.MethodDelete:
		h.mcpDelete(w, r)
	default:
		openAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST, GET, DELETE are supported", nil)
	}
}

// mcpPost 处理 POST 请求（JSON-RPC）。
func (h *Handler) mcpPost(w http.ResponseWriter, r *http.Request, routeName string, backends []store.MCPBackend) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		openAIError(w, http.StatusBadRequest, "invalid_request", "failed to read body", nil)
		return
	}

	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		openAIError(w, http.StatusBadRequest, "invalid_request", "invalid json-rpc request", nil)
		return
	}

	ctx := r.Context()
	sessionID := r.Header.Get("MCP-Session-Id")

	switch req.Method {
	case "initialize":
		h.mcpInitialize(w, r, ctx, req, routeName, backends)
	case "tools/list":
		h.mcpToolsList(w, ctx, req, sessionID, backends)
	case "tools/call":
		h.mcpToolsCall(w, ctx, req, sessionID, backends)
	default:
		// 其他方法透传到第一个可用后端
		h.mcpPassthrough(w, ctx, req, sessionID, backends)
	}
}

// mcpInitialize 处理 initialize 请求。
func (h *Handler) mcpInitialize(w http.ResponseWriter, r *http.Request, ctx context.Context, req jsonRPCRequest, routeName string, backends []store.MCPBackend) {
	var params InitializeParams
	if req.Params != nil {
		paramBytes, _ := json.Marshal(req.Params)
		_ = json.Unmarshal(paramBytes, &params)
	}

	// 生成新会话 ID
	sessionID := uuid.New().String()
	sess := &mcpSession{
		id:        sessionID,
		backends:  make(map[int64]*MCPClient),
		toolCache: make(map[string]int64),
		createdAt: time.Now(),
	}

	// 为每个后端创建客户端并发送 initialize
	var capabilities map[string]any
	var serverInfo map[string]any
	for _, b := range backends {
		if b.Status != "active" {
			continue
		}
		client := NewMCPClient(b.TargetURL, b.ApiKey, time.Duration(b.TimeoutMs)*time.Millisecond)
		result, err := client.Initialize(ctx, params)
		if err != nil {
			slog.Warn("mcp: initialize backend failed", "backend", b.Name, "err", err)
			continue
		}
		sess.backends[b.ID] = client

		// 合并 capabilities（取并集）
		if capabilities == nil {
			capabilities = result.Capabilities
		} else {
			for k, v := range result.Capabilities {
				capabilities[k] = v
			}
		}

		// 使用第一个后端的 serverInfo
		if serverInfo == nil {
			serverInfo = result.ServerInfo
		}
	}

	if len(sess.backends) == 0 {
		openAIError(w, http.StatusBadGateway, "backend_unavailable", "no backends available", nil)
		return
	}

	globalMcpSessions.set(sessionID, sess)

	// 返回聚合后的 InitializeResult
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
	}
	resultData := InitializeResult{
		ProtocolVersion: params.ProtocolVersion,
		Capabilities:    capabilities,
		ServerInfo:      serverInfo,
	}
	resultBytes, _ := json.Marshal(resultData)
	resp.Result = resultBytes

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("MCP-Session-Id", sessionID)
	_ = json.NewEncoder(w).Encode(resp)
}

// mcpToolsList 处理 tools/list 请求。
func (h *Handler) mcpToolsList(w http.ResponseWriter, ctx context.Context, req jsonRPCRequest, sessionID string, backends []store.MCPBackend) {
	sess, ok := globalMcpSessions.get(sessionID)
	if !ok {
		h.mcpError(w, req.ID, -32000, "session not found or expired")
		return
	}

	// 并发获取所有后端的工具列表
	type backendTools struct {
		backendID int64
		name      string
		tools     []Tool
		err       error
	}
	ch := make(chan backendTools, len(sess.backends))

	sess.mu.RLock()
	for backendID, client := range sess.backends {
		go func(bid int64, c *MCPClient, bname string) {
			tools, err := c.ListTools(ctx)
			ch <- backendTools{backendID: bid, name: bname, tools: tools, err: err}
		}(backendID, client, h.getBackendName(backends, backendID))
	}
	sess.mu.RUnlock()

	// 收集并聚合工具
	allTools := make([]Tool, 0)
	toolNames := make(map[string]int) // tool_name → count
	sess.mu.Lock()
	sess.toolCache = make(map[string]int64) // 清空缓存
	sess.mu.Unlock()

	for i := 0; i < len(sess.backends); i++ {
		bt := <-ch
		if bt.err != nil {
			slog.Warn("mcp: list tools failed", "backend", bt.name, "err", bt.err)
			continue
		}
		for _, tool := range bt.tools {
			toolNames[tool.Name]++
			sess.mu.Lock()
			sess.toolCache[tool.Name] = bt.backendID
			sess.mu.Unlock()
		}
		allTools = append(allTools, bt.tools...)
	}

	// 处理工具名冲突：冲突的工具加上 backend_name__ 前缀
	finalTools := make([]Tool, 0, len(allTools))
	sess.mu.RLock()
	for _, tool := range allTools {
		if toolNames[tool.Name] > 1 {
			backendID := sess.toolCache[tool.Name]
			backendName := h.getBackendName(backends, backendID)
			prefixedName := backendName + "__" + tool.Name
			tool.Name = prefixedName
			sess.mu.RUnlock()
			sess.mu.Lock()
			sess.toolCache[prefixedName] = backendID
			sess.mu.Unlock()
			sess.mu.RLock()
		}
		finalTools = append(finalTools, tool)
	}
	sess.mu.RUnlock()

	// 返回工具列表
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
	}
	resultData := map[string]any{"tools": finalTools}
	resultBytes, _ := json.Marshal(resultData)
	resp.Result = resultBytes

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// mcpToolsCall 处理 tools/call 请求。
func (h *Handler) mcpToolsCall(w http.ResponseWriter, ctx context.Context, req jsonRPCRequest, sessionID string, backends []store.MCPBackend) {
	sess, ok := globalMcpSessions.get(sessionID)
	if !ok {
		h.mcpError(w, req.ID, -32000, "session not found or expired")
		return
	}

	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if req.Params != nil {
		paramBytes, _ := json.Marshal(req.Params)
		_ = json.Unmarshal(paramBytes, &params)
	}

	// 从工具名提取 backend_name 前缀（如果有）
	toolName := params.Name
	var backendID int64
	if strings.Contains(toolName, "__") {
		parts := strings.SplitN(toolName, "__", 2)
		backendName := parts[0]
		actualToolName := parts[1]
		// 查找后端 ID
		for _, b := range backends {
			if b.Name == backendName {
				backendID = b.ID
				toolName = actualToolName
				break
			}
		}
	}

	// 如果没有前缀，从缓存中查找
	if backendID == 0 {
		sess.mu.RLock()
		backendID = sess.toolCache[params.Name]
		sess.mu.RUnlock()
	}

	// 找不到后端，尝试在所有后端中查找
	if backendID == 0 {
		h.mcpError(w, req.ID, -32601, fmt.Sprintf("tool not found: %s", params.Name))
		return
	}

	sess.mu.RLock()
	client, ok := sess.backends[backendID]
	sess.mu.RUnlock()
	if !ok {
		h.mcpError(w, req.ID, -32000, "backend not available")
		return
	}

	// 调用后端工具
	result, err := client.CallTool(ctx, toolName, params.Arguments)
	if err != nil {
		h.mcpError(w, req.ID, -32000, fmt.Sprintf("tool call failed: %v", err))
		return
	}

	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
	}
	resultBytes, _ := json.Marshal(result)
	resp.Result = resultBytes

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// mcpPassthrough 透传其他 JSON-RPC 方法到第一个可用后端。
func (h *Handler) mcpPassthrough(w http.ResponseWriter, ctx context.Context, req jsonRPCRequest, sessionID string, backends []store.MCPBackend) {
	sess, ok := globalMcpSessions.get(sessionID)
	if !ok {
		h.mcpError(w, req.ID, -32000, "session not found or expired")
		return
	}

	sess.mu.RLock()
	var client *MCPClient
	for _, c := range sess.backends {
		client = c
		break
	}
	sess.mu.RUnlock()

	if client == nil {
		h.mcpError(w, req.ID, -32000, "no backend available")
		return
	}

	// 直接透传（简化实现，暂不支持完整的 JSON-RPC 透传）
	h.mcpError(w, req.ID, -32601, "method not supported")
}

// mcpGet 处理 GET 请求（SSE 流）。
func (h *Handler) mcpGet(w http.ResponseWriter, r *http.Request, routeName string) {
	// SSE 流支持（暂不实现，返回 501）
	openAIError(w, http.StatusNotImplemented, "not_implemented", "SSE streaming not yet implemented", nil)
}

// mcpDelete 处理 DELETE 请求（清理会话）。
func (h *Handler) mcpDelete(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get("MCP-Session-Id")
	if sessionID == "" {
		openAIError(w, http.StatusBadRequest, "invalid_request", "MCP-Session-Id header required", nil)
		return
	}

	globalMcpSessions.delete(sessionID)
	w.WriteHeader(http.StatusNoContent)
}

// mcpError 返回 JSON-RPC 错误响应。
func (h *Handler) mcpError(w http.ResponseWriter, id any, code int, message string) {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &jsonRPCError{
			Code:    code,
			Message: message,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // JSON-RPC 错误仍返回 200
	_ = json.NewEncoder(w).Encode(resp)
}

// getBackendName 根据 backend_id 获取后端名称。
func (h *Handler) getBackendName(backends []store.MCPBackend, backendID int64) string {
	for _, b := range backends {
		if b.ID == backendID {
			return b.Name
		}
	}
	return fmt.Sprintf("backend_%d", backendID)
}
