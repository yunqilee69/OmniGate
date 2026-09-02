package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/cloudomni/omnigate/internal/store"
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
	TPMLimit      int64    `json:"tpm_limit"`
	BudgetUSD     float64  `json:"budget_usd"`
	BudgetReset   string   `json:"budget_reset"` // daily | monthly | never
	AllowedModels []string `json:"allowed_models"`
}

// VirtualKeyResponse 虚拟 key 响应体（包含明文 key_value）。
type VirtualKeyResponse struct {
	ID            int64    `json:"id"`
	KeyValue      string   `json:"key_value"` // 只在创建时返回完整值
	Name          string   `json:"name"`
	Status        string   `json:"status"`
	RPMLimit      int64    `json:"rpm_limit"`
	TPMLimit      int64    `json:"tpm_limit"`
	BudgetUSD     float64  `json:"budget_usd"`
	UsedUSD       float64  `json:"used_usd"`
	BudgetReset   string   `json:"budget_reset"`
	ResetAt       int64    `json:"reset_at"`
	AllowedModels []string `json:"allowed_models"`
	TotalRequests int64    `json:"total_requests"`
	LastUsedAt    int64    `json:"last_used_at"`
	CreatedAt     int64    `json:"created_at"`
	UpdatedAt     int64    `json:"updated_at"`
}

// toResponse 转换为响应体。
func toVKResponse(vk *store.VirtualKey, includeFullKey bool) VirtualKeyResponse {
	var allowed []string
	if vk.AllowedModels != "" && vk.AllowedModels != "[]" {
		json.Unmarshal([]byte(vk.AllowedModels), &allowed)
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
		TPMLimit:      vk.TPMLimit,
		BudgetUSD:     vk.BudgetUSD,
		UsedUSD:       vk.UsedUSD,
		BudgetReset:   vk.BudgetReset,
		ResetAt:       vk.ResetAt,
		AllowedModels: allowed,
		TotalRequests: vk.TotalRequests,
		LastUsedAt:    vk.LastUsedAt,
		CreatedAt:     vk.CreatedAt,
		UpdatedAt:     vk.UpdatedAt,
	}
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

	allowedModelsJSON := "[]"
	if len(req.AllowedModels) > 0 {
		b, _ := json.Marshal(req.AllowedModels)
		allowedModelsJSON = string(b)
	}

	vk := &store.VirtualKey{
		Name:          req.Name,
		Status:        "active",
		RPMLimit:      req.RPMLimit,
		TPMLimit:      req.TPMLimit,
		BudgetUSD:     req.BudgetUSD,
		BudgetReset:   req.BudgetReset,
		AllowedModels: allowedModelsJSON,
	}

	// 设置重置时间
	if req.BudgetReset == "daily" {
		vk.ResetAt = time.Now().Add(24 * time.Hour).Unix()
	} else if req.BudgetReset == "monthly" {
		vk.ResetAt = time.Now().AddDate(0, 1, 0).Unix()
	}

	if err := h.db.CreateVirtualKey(vk); err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}

	// 创建时返回完整 key_value
	writeJSON(w, 200, toVKResponse(vk, true))
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
		resp[i] = toVKResponse(&vk, false) // 列表不返回完整 key
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

	writeJSON(w, 200, toVKResponse(vk, false))
}

// UpdateVirtualKeyRequest 更新虚拟 key 请求体。
type UpdateVirtualKeyRequest struct {
	Name          *string   `json:"name"`
	Status        *string   `json:"status"`
	RPMLimit      *int64    `json:"rpm_limit"`
	TPMLimit      *int64    `json:"tpm_limit"`
	BudgetUSD     *float64  `json:"budget_usd"`
	BudgetReset   *string   `json:"budget_reset"`
	AllowedModels *[]string `json:"allowed_models"`
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
	if req.TPMLimit != nil {
		vk.TPMLimit = *req.TPMLimit
	}
	if req.BudgetUSD != nil {
		vk.BudgetUSD = *req.BudgetUSD
	}
	if req.BudgetReset != nil {
		vk.BudgetReset = *req.BudgetReset
		// 更新重置时间
		if *req.BudgetReset == "daily" {
			vk.ResetAt = time.Now().Add(24 * time.Hour).Unix()
		} else if *req.BudgetReset == "monthly" {
			vk.ResetAt = time.Now().AddDate(0, 1, 0).Unix()
		} else {
			vk.ResetAt = 0
		}
	}
	if req.AllowedModels != nil {
		b, _ := json.Marshal(*req.AllowedModels)
		vk.AllowedModels = string(b)
	}

	if err := h.db.UpdateVirtualKey(vk); err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}

	writeJSON(w, 200, toVKResponse(vk, false))
}

// Delete 删除虚拟 key。
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
	if vk.BudgetReset == "daily" {
		vk.ResetAt = time.Now().Add(24 * time.Hour).Unix()
	} else if vk.BudgetReset == "monthly" {
		vk.ResetAt = time.Now().AddDate(0, 1, 0).Unix()
	}

	if err := h.db.UpdateVirtualKey(vk); err != nil {
		writeErr(w, 500, "db_error", err.Error())
		return
	}

	writeJSON(w, 200, toVKResponse(vk, false))
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
