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

// exportConfig 导出配置的数据结构
type exportConfig struct {
	Version   string               `json:"version"`
	Providers []exportProviderData `json:"providers"`
}

type exportProviderData struct {
	Provider store.Provider `json:"provider"`
	Keys     []exportKeyData `json:"keys"`
	Models   []exportModelData `json:"models"`
}

type exportKeyData struct {
	Name     string `json:"name"`
	KeyValue string `json:"key_value"`
}

type exportModelData struct {
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	Protocol      string   `json:"protocol"`
	InputPrice    float64  `json:"input_price"`
	OutputPrice   float64  `json:"output_price"`
	PriceCurrency string   `json:"price_currency"`
	KeyNames      []string `json:"key_names"` // 使用密钥名称而不是ID
}

// exportProviders 导出所有提供商配置（包括密钥和模型）
func (s *Server) exportProviders(w http.ResponseWriter, _ *http.Request) {
	var providers []store.Provider
	if err := s.store.DB.Order("id").Find(&providers).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}

	config := exportConfig{
		Version:   "1.0",
		Providers: make([]exportProviderData, 0, len(providers)),
	}

	for _, p := range providers {
		// 获取该提供商的所有密钥
		var keys []store.ApiKey
		s.store.DB.Where("provider_id = ?", p.ID).Find(&keys)

		exportKeys := make([]exportKeyData, 0, len(keys))
		keyIDToName := make(map[int64]string)
		for _, k := range keys {
			exportKeys = append(exportKeys, exportKeyData{
				Name:     k.Name,
				KeyValue: k.KeyValue,
			})
			keyIDToName[k.ID] = k.Name
		}

		// 获取该提供商的所有模型
		var models []store.Model
		s.store.DB.Where("provider_id = ?", p.ID).Find(&models)

		exportModels := make([]exportModelData, 0, len(models))
		for _, m := range models {
			// 获取模型绑定的密钥
			var modelKeys []store.ModelKey
			s.store.DB.Where("model_id = ?", m.ID).Find(&modelKeys)

			keyNames := make([]string, 0, len(modelKeys))
			for _, mk := range modelKeys {
				if name, ok := keyIDToName[mk.KeyID]; ok {
					keyNames = append(keyNames, name)
				}
			}

			exportModels = append(exportModels, exportModelData{
				Name:          m.Name,
				Type:          m.Type,
				Protocol:      m.Protocol,
				InputPrice:    m.InputPrice,
				OutputPrice:   m.OutputPrice,
				PriceCurrency: m.PriceCurrency,
				KeyNames:      keyNames,
			})
		}

		// 清除提供商的ID和时间戳字段
		p.ID = 0
		p.CreatedAt = 0
		p.UpdatedAt = 0

		config.Providers = append(config.Providers, exportProviderData{
			Provider: p,
			Keys:     exportKeys,
			Models:   exportModels,
		})
	}

	writeJSON(w, http.StatusOK, config)
}

// importProviders 导入提供商配置
func (s *Server) importProviders(w http.ResponseWriter, r *http.Request) {
	var config exportConfig
	if err := decodeBody(r, &config); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	if config.Version != "1.0" {
		writeErr(w, http.StatusBadRequest, "unsupported_version", "unsupported config version")
		return
	}

	imported := 0
	skipped := 0
	errors := []string{}

	for _, pd := range config.Providers {
		// 检查提供商是否已存在
		var existing store.Provider
		err := s.store.DB.Where("name = ?", pd.Provider.Name).First(&existing).Error
		if err == nil {
			skipped++
			continue // 已存在，跳过
		}

		// 开始事务
		tx := s.store.DB.Begin()

		// 创建提供商
		provider := pd.Provider
		provider.ID = 0 // 确保ID为0，让数据库自动分配
		if err := tx.Create(&provider).Error; err != nil {
			tx.Rollback()
			errors = append(errors, "provider "+provider.Name+": "+err.Error())
			continue
		}

		// 创建密钥并建立名称到ID的映射
		keyNameToID := make(map[string]int64)
		for _, kd := range pd.Keys {
			key := store.ApiKey{
				ProviderID: provider.ID,
				KeyValue:   kd.KeyValue,
				Name:       kd.Name,
				Status:     "active",
			}
			if err := tx.Create(&key).Error; err != nil {
				tx.Rollback()
				errors = append(errors, "key "+kd.Name+": "+err.Error())
				goto nextProvider
			}
			keyNameToID[kd.Name] = key.ID
		}

		// 创建模型
		for _, md := range pd.Models {
			model := store.Model{
				ProviderID:    provider.ID,
				Name:          md.Name,
				Type:          md.Type,
				Protocol:      md.Protocol,
				InputPrice:    md.InputPrice,
				OutputPrice:   md.OutputPrice,
				PriceCurrency: md.PriceCurrency,
				Status:        "active",
			}
			if err := tx.Create(&model).Error; err != nil {
				tx.Rollback()
				errors = append(errors, "model "+md.Name+": "+err.Error())
				goto nextProvider
			}

			// 绑定密钥
			for _, keyName := range md.KeyNames {
				if keyID, ok := keyNameToID[keyName]; ok {
					modelKey := store.ModelKey{
						ModelID: model.ID,
						KeyID:   keyID,
					}
					if err := tx.Create(&modelKey).Error; err != nil {
						tx.Rollback()
						errors = append(errors, "model-key binding: "+err.Error())
						goto nextProvider
					}
				}
			}
		}

		tx.Commit()
		imported++

	nextProvider:
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"imported": imported,
		"skipped":  skipped,
		"errors":   errors,
	})
}
