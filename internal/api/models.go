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
	KeyIDs []int64 `json:"key_ids"`
}

var validProtocols = map[string]bool{"openai": true, "responses": true, "anthropic": true}
var validCurrencies = map[string]bool{"USD": true, "CNY": true}
var validModelTypes = map[string]bool{"chat": true, "embedding": true, "rerank": true}

type modelCreateReq struct {
	ProviderID    int64   `json:"provider_id"`
	Name          string  `json:"name"`
	Type          string  `json:"type"`
	Protocol      string  `json:"protocol"`
	InputPrice    float64 `json:"input_price"`
	OutputPrice   float64 `json:"output_price"`
	PriceCurrency string  `json:"price_currency"`
	KeyIDs        []int64 `json:"key_ids"`
}

func (s *Server) listModels(w http.ResponseWriter, _ *http.Request) {
	var models []store.Model
	if err := s.store.DB.Order("id").Find(&models).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	var mks []store.ModelKey
	if err := s.store.DB.Find(&mks).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	byModel := map[int64][]int64{}
	for _, mk := range mks {
		byModel[mk.ModelID] = append(byModel[mk.ModelID], mk.KeyID)
	}
	out := make([]modelResp, 0, len(models))
	for _, m := range models {
		ids := byModel[m.ID]
		if ids == nil {
			ids = []int64{}
		}
		out = append(out, modelResp{Model: m, KeyIDs: ids})
	}
	writeJSON(w, http.StatusOK, out)
}

// validateKeysBelongToProvider 校验密钥存在且均属于指定提供商（密钥物理上无法服务另一提供商的模型）。
func (s *Server) validateKeysBelongToProvider(keyIDs []int64, providerID int64) (bool, string) {
	if len(keyIDs) == 0 {
		return true, ""
	}
	var keys []store.ApiKey
	if err := s.store.DB.Where("id IN ?", keyIDs).Find(&keys).Error; err != nil {
		return false, err.Error()
	}
	if len(keys) != len(uniqueInt64(keyIDs)) {
		return false, "one or more keys do not exist"
	}
	for _, k := range keys {
		if k.ProviderID != providerID {
			return false, "key '" + maskKey(k.KeyValue) + "' belongs to a different provider"
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
	if req.Type == "" {
		req.Type = "chat"
	}
	if !validModelTypes[req.Type] {
		writeErr(w, http.StatusBadRequest, "bad_request", "type must be chat, embedding or rerank")
		return
	}
	// embedding/rerank 出站固定 OpenAI 风格直通（业界无可归一标准），不支持协议转换
	if req.Type != "chat" && req.Protocol != "openai" {
		writeErr(w, http.StatusBadRequest, "bad_request", "embedding/rerank 模型仅支持 openai 协议")
		return
	}
	if req.PriceCurrency == "" {
		req.PriceCurrency = "USD"
	}
	if !validCurrencies[req.PriceCurrency] {
		writeErr(w, http.StatusBadRequest, "bad_request", "price_currency must be USD or CNY")
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
	if ok, msg := s.validateKeysBelongToProvider(req.KeyIDs, req.ProviderID); !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", msg)
		return
	}
	if len(req.KeyIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "至少绑定一个密钥（模型必须通过密钥访问上游）")
		return
	}
	m := store.Model{
		ProviderID: req.ProviderID, Name: req.Name, Type: req.Type, Protocol: req.Protocol,
		InputPrice: req.InputPrice, OutputPrice: req.OutputPrice, PriceCurrency: req.PriceCurrency, Status: "active",
	}
	err := s.store.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&m).Error; err != nil {
			return err
		}
		for _, kid := range uniqueInt64(req.KeyIDs) {
			if err := tx.Create(&store.ModelKey{ModelID: m.ID, KeyID: kid}).Error; err != nil {
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
	ids := uniqueInt64(req.KeyIDs)
	if ids == nil {
		ids = []int64{}
	}
	writeJSON(w, http.StatusCreated, modelResp{Model: m, KeyIDs: ids})
}

type modelUpdateReq struct {
	ProviderID    *int64   `json:"provider_id"`
	Name          *string  `json:"name"`
	Type          *string  `json:"type"`
	Protocol      *string  `json:"protocol"`
	InputPrice    *float64 `json:"input_price"`
	OutputPrice   *float64 `json:"output_price"`
	PriceCurrency *string  `json:"price_currency"`
	KeyIDs        []int64  `json:"key_ids"`
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
	if req.Type != nil {
		if !validModelTypes[*req.Type] {
			writeErr(w, http.StatusBadRequest, "bad_request", "type must be chat, embedding or rerank")
			return
		}
		simple["type"] = *req.Type
	}
	// 组合校验：type 与 protocol 以「更新后生效值」判断，防止单边更新漏过非法组合
	effType, effProto := m.Type, m.Protocol
	if req.Type != nil {
		effType = *req.Type
	}
	if req.Protocol != nil {
		effProto = *req.Protocol
	}
	if effType != "" && effType != "chat" && effProto != "openai" {
		writeErr(w, http.StatusBadRequest, "bad_request", "embedding/rerank 模型仅支持 openai 协议")
		return
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
	if req.PriceCurrency != nil {
		if !validCurrencies[*req.PriceCurrency] {
			writeErr(w, http.StatusBadRequest, "bad_request", "price_currency must be USD or CNY")
			return
		}
		simple["price_currency"] = *req.PriceCurrency
	}
	if len(simple) == 0 && req.KeyIDs == nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "no fields to update")
		return
	}
	if req.KeyIDs != nil {
		if ok, msg := s.validateKeysBelongToProvider(req.KeyIDs, m.ProviderID); !ok {
			writeErr(w, http.StatusBadRequest, "bad_request", msg)
			return
		}
		if len(req.KeyIDs) == 0 {
			writeErr(w, http.StatusBadRequest, "bad_request", "至少绑定一个密钥（清空绑定请直接删除模型）")
			return
		}
	}
	err := s.store.DB.Transaction(func(tx *gorm.DB) error {
		if len(simple) > 0 {
			if err := tx.Model(&m).Updates(simple).Error; err != nil {
				return err
			}
		}
		if req.KeyIDs != nil {
			if err := tx.Where("model_id = ?", id).Delete(&store.ModelKey{}).Error; err != nil {
				return err
			}
			for _, kid := range uniqueInt64(req.KeyIDs) {
				if err := tx.Create(&store.ModelKey{ModelID: id, KeyID: kid}).Error; err != nil {
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
	var keyIDs []int64
	_ = s.store.DB.Model(&store.ModelKey{}).Where("model_id = ?", id).Pluck("key_id", &keyIDs).Error
	if keyIDs == nil {
		keyIDs = []int64{}
	}
	writeJSON(w, http.StatusOK, modelResp{Model: m, KeyIDs: keyIDs})
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
		if err := tx.Where("model_id = ?", id).Delete(&store.ModelKey{}).Error; err != nil {
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
