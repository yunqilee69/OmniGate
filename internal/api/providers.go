package api

import (
	"errors"
	"net/http"
	"strings"

	"gorm.io/gorm"

	"github.com/cloudomni/omnigate/internal/store"
)

type providerCreateReq struct {
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	Protocol  string `json:"protocol"`
	ProxyURL  string `json:"proxy_url"`
	TimeoutMs int    `json:"timeout_ms"`
	Remark    string `json:"remark"`
}

func (s *Server) listProviders(w http.ResponseWriter, _ *http.Request) {
	var out []store.Provider
	if err := s.store.DB.Order("id").Find(&out).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createProvider(w http.ResponseWriter, r *http.Request) {
	var req providerCreateReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.BaseURL = strings.TrimSpace(req.BaseURL)
	req.ProxyURL = strings.TrimSpace(req.ProxyURL)
	if req.Name == "" || req.BaseURL == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "name and base_url are required")
		return
	}
	if req.Protocol == "" {
		req.Protocol = "openai"
	}
	if req.Protocol != "openai" && req.Protocol != "anthropic" {
		writeErr(w, http.StatusBadRequest, "bad_request", "protocol must be openai or anthropic")
		return
	}
	if req.TimeoutMs <= 0 {
		req.TimeoutMs = 120000
	}
	p := store.Provider{
		Name: req.Name, BaseURL: req.BaseURL, Protocol: req.Protocol,
		ProxyURL: req.ProxyURL, TimeoutMs: req.TimeoutMs, Remark: req.Remark,
	}
	if err := s.store.DB.Create(&p).Error; err != nil {
		if isUniqueErr(err) {
			writeErr(w, http.StatusConflict, "conflict", "provider name already exists")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

type providerUpdateReq struct {
	Name      *string `json:"name"`
	BaseURL   *string `json:"base_url"`
	Protocol  *string `json:"protocol"`
	ProxyURL  *string `json:"proxy_url"`
	TimeoutMs *int    `json:"timeout_ms"`
	Remark    *string `json:"remark"`
}

func (s *Server) updateProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	var req providerUpdateReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	var p store.Provider
	if err := s.store.DB.First(&p, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "provider not found")
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
		updates["name"] = v
	}
	if req.BaseURL != nil {
		v := strings.TrimSpace(*req.BaseURL)
		if v == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "base_url must not be empty")
			return
		}
		updates["base_url"] = v
	}
	if req.Protocol != nil {
		if *req.Protocol != "openai" && *req.Protocol != "anthropic" {
			writeErr(w, http.StatusBadRequest, "bad_request", "protocol must be openai or anthropic")
			return
		}
		updates["protocol"] = *req.Protocol
	}
	if req.ProxyURL != nil {
		updates["proxy_url"] = strings.TrimSpace(*req.ProxyURL)
	}
	if req.TimeoutMs != nil {
		if *req.TimeoutMs <= 0 {
			writeErr(w, http.StatusBadRequest, "bad_request", "timeout_ms must be positive")
			return
		}
		updates["timeout_ms"] = *req.TimeoutMs
	}
	if req.Remark != nil {
		updates["remark"] = *req.Remark
	}
	if len(updates) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "no fields to update")
		return
	}
	if err := s.store.DB.Model(&p).Updates(updates).Error; err != nil {
		if isUniqueErr(err) {
			writeErr(w, http.StatusConflict, "conflict", "provider name already exists")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	_ = s.store.DB.First(&p, id).Error
	if invalidator, ok := s.chat.(interface{ InvalidateProviderCache(int64) }); ok {
		invalidator.InvalidateProviderCache(id)
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) deleteProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	var p store.Provider
	if err := s.store.DB.First(&p, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "provider not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	// 级联：密钥（及模型-密钥绑定）→ 模型（及路由目标、模型-密钥绑定）→ 提供商
	err := s.store.DB.Transaction(func(tx *gorm.DB) error {
		var keyIDs []int64
		if err := tx.Model(&store.ApiKey{}).Where("provider_id = ?", id).Pluck("id", &keyIDs).Error; err != nil {
			return err
		}
		if len(keyIDs) > 0 {
			if err := tx.Where("key_id IN ?", keyIDs).Delete(&store.ModelKey{}).Error; err != nil {
				return err
			}
			if err := tx.Where("id IN ?", keyIDs).Delete(&store.ApiKey{}).Error; err != nil {
				return err
			}
		}
		var modelIDs []int64
		if err := tx.Model(&store.Model{}).Where("provider_id = ?", id).Pluck("id", &modelIDs).Error; err != nil {
			return err
		}
		if len(modelIDs) > 0 {
			if err := tx.Where("model_id IN ?", modelIDs).Delete(&store.RouteTarget{}).Error; err != nil {
				return err
			}
			if err := tx.Where("model_id IN ?", modelIDs).Delete(&store.ModelKey{}).Error; err != nil {
				return err
			}
			if err := tx.Where("provider_id = ?", id).Delete(&store.Model{}).Error; err != nil {
				return err
			}
		}
		return tx.Delete(&store.Provider{}, id).Error
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if invalidator, ok := s.chat.(interface{ InvalidateProviderCache(int64) }); ok {
		invalidator.InvalidateProviderCache(id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}
