package api

import (
	"errors"
	"net/http"
	"strings"

	"gorm.io/gorm"

	"github.com/cloudomni/omnigate/internal/store"
)

type modelResp struct {
	store.Model
	PoolIDs []int64 `json:"pool_ids"`
}

var validProtocols = map[string]bool{"openai": true, "responses": true, "anthropic": true}

type modelCreateReq struct {
	ProviderID  int64   `json:"provider_id"`
	Name        string  `json:"name"`
	Protocol    string  `json:"protocol"`
	InputPrice  float64 `json:"input_price"`
	OutputPrice float64 `json:"output_price"`
	PoolIDs     []int64 `json:"pool_ids"`
}

func (s *Server) listModels(w http.ResponseWriter, _ *http.Request) {
	var models []store.Model
	if err := s.store.DB.Order("id").Find(&models).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	var mps []store.ModelPool
	if err := s.store.DB.Find(&mps).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	byModel := map[int64][]int64{}
	for _, mp := range mps {
		byModel[mp.ModelID] = append(byModel[mp.ModelID], mp.PoolID)
	}
	out := make([]modelResp, 0, len(models))
	for _, m := range models {
		ids := byModel[m.ID]
		if ids == nil {
			ids = []int64{}
		}
		out = append(out, modelResp{Model: m, PoolIDs: ids})
	}
	writeJSON(w, http.StatusOK, out)
}

// validatePoolsBelongToProvider 校验池存在且均属于指定提供商（密钥物理上无法服务另一提供商的模型）。
func (s *Server) validatePoolsBelongToProvider(poolIDs []int64, providerID int64) (bool, string) {
	if len(poolIDs) == 0 {
		return true, ""
	}
	var pools []store.KeyPool
	if err := s.store.DB.Where("id IN ?", poolIDs).Find(&pools).Error; err != nil {
		return false, err.Error()
	}
	if len(pools) != len(uniqueInt64(poolIDs)) {
		return false, "one or more pools do not exist"
	}
	for _, p := range pools {
		if p.ProviderID != providerID {
			return false, "pool '" + p.Name + "' belongs to a different provider"
		}
	}
	return true, ""
}

func uniqueInt64(xs []int64) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(xs))
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func (s *Server) createModel(w http.ResponseWriter, r *http.Request) {
	var req modelCreateReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.ProviderID <= 0 || req.Name == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "provider_id and name are required")
		return
	}
	if req.Protocol == "" {
		req.Protocol = "openai"
	}
	if !validProtocols[req.Protocol] {
		writeErr(w, http.StatusBadRequest, "bad_request", "protocol must be openai, responses or anthropic")
		return
	}
	if req.InputPrice < 0 || req.OutputPrice < 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "prices must not be negative")
		return
	}
	var provider store.Provider
	if err := s.store.DB.First(&provider, req.ProviderID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "provider not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if ok, msg := s.validatePoolsBelongToProvider(req.PoolIDs, req.ProviderID); !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", msg)
		return
	}
	if len(req.PoolIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "至少绑定一个密钥池（模型必须通过某个池的密钥访问上游）")
		return
	}
	m := store.Model{
		ProviderID: req.ProviderID, Name: req.Name, Protocol: req.Protocol,
		InputPrice: req.InputPrice, OutputPrice: req.OutputPrice, Status: "active",
	}
	err := s.store.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&m).Error; err != nil {
			return err
		}
		for _, pid := range uniqueInt64(req.PoolIDs) {
			if err := tx.Create(&store.ModelPool{ModelID: m.ID, PoolID: pid}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if isUniqueErr(err) {
			writeErr(w, http.StatusConflict, "conflict", "model name already exists under this provider")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	ids := uniqueInt64(req.PoolIDs)
	if ids == nil {
		ids = []int64{}
	}
	writeJSON(w, http.StatusCreated, modelResp{Model: m, PoolIDs: ids})
}

type modelUpdateReq struct {
	ProviderID  *int64   `json:"provider_id"`
	Name        *string  `json:"name"`
	Protocol    *string  `json:"protocol"`
	InputPrice  *float64 `json:"input_price"`
	OutputPrice *float64 `json:"output_price"`
	PoolIDs     []int64  `json:"pool_ids"`
}

func (s *Server) updateModel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	var req modelUpdateReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	var m store.Model
	if err := s.store.DB.First(&m, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "model not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if req.ProviderID != nil && *req.ProviderID != m.ProviderID {
		writeErr(w, http.StatusBadRequest, "bad_request", "changing model provider is not allowed; delete and recreate the model")
		return
	}
	simple := map[string]any{}
	if req.Name != nil {
		v := strings.TrimSpace(*req.Name)
		if v == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "name must not be empty")
			return
		}
		simple["name"] = v
	}
	if req.Protocol != nil {
		if !validProtocols[*req.Protocol] {
			writeErr(w, http.StatusBadRequest, "bad_request", "protocol must be openai, responses or anthropic")
			return
		}
		simple["protocol"] = *req.Protocol
	}
	if req.InputPrice != nil {
		if *req.InputPrice < 0 {
			writeErr(w, http.StatusBadRequest, "bad_request", "input_price must not be negative")
			return
		}
		simple["input_price"] = *req.InputPrice
	}
	if req.OutputPrice != nil {
		if *req.OutputPrice < 0 {
			writeErr(w, http.StatusBadRequest, "bad_request", "output_price must not be negative")
			return
		}
		simple["output_price"] = *req.OutputPrice
	}
	if len(simple) == 0 && req.PoolIDs == nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "no fields to update")
		return
	}
	if req.PoolIDs != nil {
		if ok, msg := s.validatePoolsBelongToProvider(req.PoolIDs, m.ProviderID); !ok {
			writeErr(w, http.StatusBadRequest, "bad_request", msg)
			return
		}
		if len(req.PoolIDs) == 0 {
			writeErr(w, http.StatusBadRequest, "bad_request", "至少绑定一个密钥池（清空绑定请直接删除模型）")
			return
		}
	}
	err := s.store.DB.Transaction(func(tx *gorm.DB) error {
		if len(simple) > 0 {
			if err := tx.Model(&m).Updates(simple).Error; err != nil {
				return err
			}
		}
		if req.PoolIDs != nil {
			if err := tx.Where("model_id = ?", id).Delete(&store.ModelPool{}).Error; err != nil {
				return err
			}
			for _, pid := range uniqueInt64(req.PoolIDs) {
				if err := tx.Create(&store.ModelPool{ModelID: id, PoolID: pid}).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		if isUniqueErr(err) {
			writeErr(w, http.StatusConflict, "conflict", "model name already exists under this provider")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	_ = s.store.DB.First(&m, id).Error
	var poolIDs []int64
	_ = s.store.DB.Model(&store.ModelPool{}).Where("model_id = ?", id).Pluck("pool_id", &poolIDs).Error
	if poolIDs == nil {
		poolIDs = []int64{}
	}
	writeJSON(w, http.StatusOK, modelResp{Model: m, PoolIDs: poolIDs})
}

func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	var m store.Model
	if err := s.store.DB.First(&m, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "model not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	err := s.store.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("model_id = ?", id).Delete(&store.RouteTarget{}).Error; err != nil {
			return err
		}
		if err := tx.Where("model_id = ?", id).Delete(&store.ModelPool{}).Error; err != nil {
			return err
		}
		return tx.Delete(&store.Model{}, id).Error
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// enableModel 手动解禁：重置熔断状态机。
func (s *Server) enableModel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	var m store.Model
	if err := s.store.DB.First(&m, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "model not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if err := s.store.DB.Model(&m).Updates(map[string]any{
		"status": "active", "fail_count": 0, "cooldown_until": 0, "disable_reason": "",
	}).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	_ = s.store.DB.First(&m, id).Error
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) disableModel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	var m store.Model
	if err := s.store.DB.First(&m, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "model not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if err := s.store.DB.Model(&m).Updates(map[string]any{
		"status": "disabled", "disable_reason": "manually disabled via admin API",
	}).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	_ = s.store.DB.First(&m, id).Error
	writeJSON(w, http.StatusOK, m)
}
