package api

import (
	"errors"
	"net/http"
	"strings"

	"gorm.io/gorm"

	"github.com/cloudomni/omnigate/internal/store"
)

type mcpBackendCreateReq struct {
	Name      string `json:"name"`
	TargetURL string `json:"target_url"`
	ApiKey    string `json:"api_key"`
	TimeoutMs int    `json:"timeout_ms"`
	Remark    string `json:"remark"`
}

func (s *Server) listMcpBackends(w http.ResponseWriter, _ *http.Request) {
	var out []store.MCPBackend
	if err := s.store.DB.Order("id").Find(&out).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getMcpBackend(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	var b store.MCPBackend
	if err := s.store.DB.First(&b, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "mcp backend not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) createMcpBackend(w http.ResponseWriter, r *http.Request) {
	var req mcpBackendCreateReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.TargetURL = strings.TrimSpace(req.TargetURL)
	if req.Name == "" || req.TargetURL == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "name and target_url are required")
		return
	}
	// MCP backend name 不能包含 "__"（用于工具名冲突时的前缀分隔符）
	if strings.Contains(req.Name, "__") {
		writeErr(w, http.StatusBadRequest, "bad_request", "name must not contain '__'")
		return
	}
	if req.TimeoutMs <= 0 {
		req.TimeoutMs = 30000
	}
	b := store.MCPBackend{
		Name: req.Name, TargetURL: req.TargetURL, ApiKey: req.ApiKey,
		TimeoutMs: req.TimeoutMs, Remark: req.Remark,
	}
	if err := s.store.DB.Create(&b).Error; err != nil {
		if isUniqueErr(err) {
			writeErr(w, http.StatusConflict, "conflict", "mcp backend name already exists")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

type mcpBackendUpdateReq struct {
	Name      *string `json:"name"`
	TargetURL *string `json:"target_url"`
	ApiKey    *string `json:"api_key"`
	TimeoutMs *int    `json:"timeout_ms"`
	Status    *string `json:"status"`
	Remark    *string `json:"remark"`
}

func (s *Server) updateMcpBackend(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	var req mcpBackendUpdateReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	var b store.MCPBackend
	if err := s.store.DB.First(&b, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "mcp backend not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	updates := map[string]any{}
	if req.Name != nil {
		v := strings.TrimSpace(*req.Name)
		if v == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "name must not be empty")
			return
		}
		if strings.Contains(v, "__") {
			writeErr(w, http.StatusBadRequest, "bad_request", "name must not contain '__'")
			return
		}
		updates["name"] = v
	}
	if req.TargetURL != nil {
		updates["target_url"] = strings.TrimSpace(*req.TargetURL)
	}
	if req.ApiKey != nil {
		updates["api_key"] = *req.ApiKey
	}
	if req.TimeoutMs != nil {
		updates["timeout_ms"] = *req.TimeoutMs
	}
	if req.Status != nil {
		v := *req.Status
		if v != "active" && v != "disabled" {
			writeErr(w, http.StatusBadRequest, "bad_request", "status must be active or disabled")
			return
		}
		updates["status"] = v
	}
	if req.Remark != nil {
		updates["remark"] = *req.Remark
	}
	if len(updates) == 0 {
		writeJSON(w, http.StatusOK, b)
		return
	}
	if err := s.store.DB.Model(&b).Updates(updates).Error; err != nil {
		if isUniqueErr(err) {
			writeErr(w, http.StatusConflict, "conflict", "mcp backend name already exists")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	_ = s.store.DB.First(&b, id).Error
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) deleteMcpBackend(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	// 检查是否有路由引用
	var count int64
	if err := s.store.DB.Model(&store.RouteMcpTarget{}).Where("mcp_backend_id = ?", id).Count(&count).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if count > 0 {
		writeErr(w, http.StatusConflict, "conflict", "mcp backend is referenced by one or more routes, remove references first")
		return
	}
	if err := s.store.DB.Delete(&store.MCPBackend{}, id).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}