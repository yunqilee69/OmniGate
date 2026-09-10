package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/cloudomni/omnigate/internal/store"
	"github.com/go-chi/chi/v5"
)

// VirtualKeyHandler 虚拟密钥管理接口。
type VirtualKeyHandler struct {
	db *store.Store
}

func NewVirtualKeyHandler(db *store.Store) *VirtualKeyHandler {
	return &VirtualKeyHandler{db: db}
}

// CreateVirtualKeyRequest 创建虚拟 key 请求体。
type CreateVirtualKeyRequest struct {
	Name          string   `json:"name"`
	RPMLimit      int64    `json:"rpm_limit"`
	TotalBudget   float64  `json:"total_budget"`
	AllowedRoutes []string `json:"allowed_routes"`
}

// VirtualKeyResponse 虚拟 key 响应体（包含明文 key_value）。
type VirtualKeyResponse struct {
	ID            int64    `json:"id"`
	KeyValue      string   `json:"key_value"` // 只在创建时返回完整值
	Name          string   `json:"name"`
	Status        string   `json:"status"`
	RPMLimit      int64    `json:"rpm_limit"`
	TotalBudget   float64  `json:"total_budget"`
	UsedUSD       float64  `json:"used_usd"`
	AllowedRoutes []string `json:"allowed_routes"`
	TotalRequests int64    `json:"total_requests"`
	LastUsedAt    int64    `json:"last_used_at"`
	CreatedAt     int64    `json:"created_at"`
	UpdatedAt     int64    `json:"updated_at"`
}

// toVKResponse 转换为响应体。API 契约为路由名：DB 存 ID 数组，响应还原为名称
// （已删除的路由 ID 跳过）。
func (h *VirtualKeyHandler) toVKResponse(vk *store.VirtualKey, includeFullKey bool) VirtualKeyResponse {
	var allowed []string
	if vk.AllowedRoutes != "" && vk.AllowedRoutes != "[]" {
		var ids []int64
		if json.Unmarshal([]byte(vk.AllowedRoutes), &ids) == nil && len(ids) > 0 {
			byID := h.routeNameMap()
			allowed = make([]string, 0, len(ids))
			for _, id := range ids {
				if name, ok := byID[id]; ok {
					allowed = append(allowed, name)
				}
			}
		}
	}

	keyValue := vk.KeyValue
	if !includeFullKey && len(keyValue) > 12 {
		keyValue = keyValue[:12] + "..." // 脱敏显示
	}

	return VirtualKeyResponse{
		ID:            vk.ID,
		KeyValue:      keyValue,
		Name:          vk.Name,
		Status:        vk.Status,
		RPMLimit:      vk.RPMLimit,
		TotalBudget:   vk.TotalBudgetUSD,
		UsedUSD:       vk.UsedUSD,
		AllowedRoutes: allowed,
		TotalRequests: vk.TotalRequests,
		LastUsedAt:    vk.LastUsedAt,
		CreatedAt:     vk.CreatedAt,
		UpdatedAt:     vk.UpdatedAt,
	}
}

// routeNameMap 路由 ID → 名称映射；读取失败返回空映射（allowed_routes 显示为空）。
func (h *VirtualKeyHandler) routeNameMap() map[int64]string {
	var routes []store.Route
	if err := h.db.DB.Find(&routes).Error; err != nil {
		return map[int64]string{}
	}
	m := make(map[int64]string, len(routes))
	for _, rt := range routes {
		m[rt.ID] = rt.Name
	}
	return m
}

// resolveRouteNames 把路由名解析为 ID；未知名报错。DB 中 allowed_routes 统一存 ID 数组。
func (h *VirtualKeyHandler) resolveRouteNames(names []string) ([]int64, error) {
	var routes []store.Route
	if err := h.db.DB.Find(&routes).Error; err != nil {
		return nil, err
	}
	byName := make(map[string]int64, len(routes))
	for _, rt := range routes {
		byName[rt.Name] = rt.ID
	}
	ids := make([]int64, 0, len(names))
	for _, n := range names {
		id, ok := byName[n]
		if !ok {
			return nil, fmt.Errorf("unknown route '%s'", n)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// Create 创建虚拟 key。
func (h *VirtualKeyHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req CreateVirtualKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid_json", err.Error())
		return
	}

	if req.Name == "" {
		writeErr(w, 400, "invalid_request", "name is required")
		return
	}

	allowedRoutesJSON := "[]"
	if len(req.AllowedRoutes) > 0 {
		ids, err := h.resolveRouteNames(req.AllowedRoutes)
		if err != nil {
			writeErr(w, 400, "unknown_route", err.Error())
			return
		}
		b, _ := json.Marshal(ids)
		allowedRoutesJSON = string(b)
	}

	vk := &store.VirtualKey{
		Name:           req.Name,
		Status:         "active",
		RPMLimit:       req.RPMLimit,
		TotalBudgetUSD: req.TotalBudget,
		AllowedRoutes:  allowedRoutesJSON,
	}

	if err := h.db.CreateVirtualKey(vk); err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}

	// 创建时返回完整 key_value
	writeJSON(w, 200, h.toVKResponse(vk, true))
}

// List 列出所有虚拟 key。
func (h *VirtualKeyHandler) List(w http.ResponseWriter, r *http.Request) {
	vks, err := h.db.ListVirtualKeys()
	if err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}

	resp := make([]VirtualKeyResponse, len(vks))
	for i, vk := range vks {
		resp[i] = h.toVKResponse(&vk, false) // 列表不返回完整 key
	}
	writeJSON(w, 200, resp)
}

// Get 获取单个虚拟 key。
func (h *VirtualKeyHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid_id", "id must be a positive integer")
		return
	}

	vk, err := h.db.GetVirtualKey(id)
	if err != nil {
		writeErr(w, 404, "not_found", "virtual key not found")
		return
	}

	writeJSON(w, 200, h.toVKResponse(vk, false))
}

// UpdateVirtualKeyRequest 更新虚拟 key 请求体。
type UpdateVirtualKeyRequest struct {
	Name          *string   `json:"name"`
	Status        *string   `json:"status"`
	RPMLimit      *int64    `json:"rpm_limit"`
	TotalBudget   *float64  `json:"total_budget"`
	AllowedRoutes *[]string `json:"allowed_routes"`
}

func (h *VirtualKeyHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid_id", "id must be a positive integer")
		return
	}

	vk, err := h.db.GetVirtualKey(id)
	if err != nil {
		writeErr(w, 404, "not_found", "virtual key not found")
		return
	}

	var req UpdateVirtualKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid_json", err.Error())
		return
	}

	if req.Name != nil {
		vk.Name = *req.Name
	}
	if req.Status != nil {
		vk.Status = *req.Status
	}
	if req.RPMLimit != nil {
		vk.RPMLimit = *req.RPMLimit
	}
	if req.TotalBudget != nil {
		vk.TotalBudgetUSD = *req.TotalBudget
	}
	if req.AllowedRoutes != nil {
		ids, err := h.resolveRouteNames(*req.AllowedRoutes)
		if err != nil {
			writeErr(w, 400, "unknown_route", err.Error())
			return
		}
		b, _ := json.Marshal(ids)
		vk.AllowedRoutes = string(b)
	}

	if err := h.db.UpdateVirtualKey(vk); err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}

	writeJSON(w, 200, h.toVKResponse(vk, false))
}
func (h *VirtualKeyHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid_id", "id must be a positive integer")
		return
	}

	if err := h.db.DeleteVirtualKey(id); err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}

	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// ResetBudget 手动重置虚拟 key 配额。
func (h *VirtualKeyHandler) ResetBudget(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid_id", "id must be a positive integer")
		return
	}

	vk, err := h.db.GetVirtualKey(id)
	if err != nil {
		writeErr(w, 404, "not_found", "virtual key not found")
		return
	}

	vk.UsedUSD = 0

	if err := h.db.UpdateVirtualKey(vk); err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}

	writeJSON(w, 200, h.toVKResponse(vk, false))
}

// RevealKey 查看完整密钥（仅用于需要时查看）。
func (h *VirtualKeyHandler) RevealKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid_id", "id must be a positive integer")
		return
	}

	vk, err := h.db.GetVirtualKey(id)
	if err != nil {
		writeErr(w, 404, "not_found", "virtual key not found")
		return
	}

	writeJSON(w, 200, map[string]string{"key_value": vk.KeyValue})
}
