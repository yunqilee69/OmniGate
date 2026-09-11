package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/cloudomni/omnigate/internal/breaker"
	"github.com/cloudomni/omnigate/internal/store"
)

type modelResp struct {
	store.Model
	KeyIDs     []int64          `json:"key_ids"`
	BannedKeys map[int64]string `json:"banned_keys"` // 组合禁用 key_id → 原因（手动或失败归因）
}

var validProtocols = map[string]bool{"completions": true, "responses": true, "messages": true}
var validCurrencies = map[string]bool{"USD": true, "CNY": true}
var validModelTypes = map[string]bool{"chat": true, "embedding": true, "rerank": true, "image": true}

type modelCreateReq struct {
	ProviderID    int64   `json:"provider_id"`
	Name          string  `json:"name"`
	Type          string  `json:"type"`
	Protocol      string  `json:"protocol"`
	InputPrice    float64 `json:"input_price"`
	OutputPrice   float64 `json:"output_price"`
	PriceCurrency string  `json:"price_currency"`
	KeyIDs        []int64 `json:"key_ids"`
	ApiPath       string  `json:"api_path"`
	BodyOverride  string  `json:"body_override"`
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
	now := time.Now().Unix()
	bansByModel := map[int64]map[int64]string{}
	var bans []store.ModelKeyBan
	if err := s.store.DB.Find(&bans).Error; err == nil {
		for _, b := range bans {
			if b.Status == "temp_banned" && b.BannedUntil <= now {
				continue // 过期 temp_banned 半开可用，不算禁用
			}
			m := bansByModel[b.ModelID]
			if m == nil {
				m = map[int64]string{}
				bansByModel[b.ModelID] = m
			}
			m[b.KeyID] = b.BanReason
		}
	}
	out := make([]modelResp, 0, len(models))
	for _, m := range models {
		ids := byModel[m.ID]
		if ids == nil {
			ids = []int64{}
		}
		banned := bansByModel[m.ID]
		if banned == nil {
			banned = map[int64]string{}
		}
		out = append(out, modelResp{Model: m, KeyIDs: ids, BannedKeys: banned})
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
		req.Protocol = "completions"
	}
	if !validProtocols[req.Protocol] {
		writeErr(w, http.StatusBadRequest, "bad_request", "protocol must be completions, responses or messages")
		return
	}
	if req.Type == "" {
		req.Type = "chat"
	}
	if !validModelTypes[req.Type] {
		writeErr(w, http.StatusBadRequest, "bad_request", "type must be chat, embedding, rerank or image")
		return
	}
	// 非 chat 类型出站固定 completions 风格直通（业界无可归一标准），不支持协议转换
	if req.Type != "chat" && req.Protocol != "completions" {
		writeErr(w, http.StatusBadRequest, "bad_request", "embedding/rerank/image 模型仅支持 completions 协议")
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
	// 验证 ApiPath 格式
	if req.ApiPath != "" {
		if !strings.HasPrefix(req.ApiPath, "/") && !strings.HasPrefix(req.ApiPath, "http://") && !strings.HasPrefix(req.ApiPath, "https://") {
			writeErr(w, http.StatusBadRequest, "bad_request", "api_path 必须以 / 或 http:// 或 https:// 开头")
			return
		}
	}
	// 验证 BodyOverride 格式
	if req.BodyOverride != "" {
		var tmp map[string]any
		if err := json.Unmarshal([]byte(req.BodyOverride), &tmp); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "body_override 必须是有效的 JSON 对象")
			return
		}
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
		ApiPath: req.ApiPath, BodyOverride: req.BodyOverride,
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
	ApiPath       *string  `json:"api_path"`
	BodyOverride  *string  `json:"body_override"`
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
			writeErr(w, http.StatusBadRequest, "bad_request", "protocol must be completions, responses or messages")
			return
		}
		simple["protocol"] = *req.Protocol
	}
	if req.Type != nil {
		if !validModelTypes[*req.Type] {
			writeErr(w, http.StatusBadRequest, "bad_request", "type must be chat, embedding, rerank or image")
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
	if effType != "" && effType != "chat" && effProto != "completions" {
		writeErr(w, http.StatusBadRequest, "bad_request", "embedding/rerank/image 模型仅支持 completions 协议")
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
	if req.ApiPath != nil {
		if *req.ApiPath != "" {
			if !strings.HasPrefix(*req.ApiPath, "/") && !strings.HasPrefix(*req.ApiPath, "http://") && !strings.HasPrefix(*req.ApiPath, "https://") {
				writeErr(w, http.StatusBadRequest, "bad_request", "api_path 必须以 / 或 http:// 或 https:// 开头")
				return
			}
		}
		simple["api_path"] = *req.ApiPath
	}
	if req.BodyOverride != nil {
		if *req.BodyOverride != "" {
			var tmp map[string]any
			if err := json.Unmarshal([]byte(*req.BodyOverride), &tmp); err != nil {
				writeErr(w, http.StatusBadRequest, "bad_request", "body_override 必须是有效的 JSON 对象")
				return
			}
		}
		simple["body_override"] = *req.BodyOverride
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
	banned := map[int64]string{}
	var bans []store.ModelKeyBan
	if err := s.store.DB.Where("model_id = ?", id).Find(&bans).Error; err == nil {
		for _, b := range bans {
			banned[b.KeyID] = b.BanReason
		}
	}
	writeJSON(w, http.StatusOK, modelResp{Model: m, KeyIDs: keyIDs, BannedKeys: banned})
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
		if err := tx.Where("model_id = ?", id).Delete(&store.ModelKeyBan{}).Error; err != nil {
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

// enableModel 手动解禁：清除该模型全部组合禁用。
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
	if err := breaker.New(s.store).UnbanAllModelKeys(m.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "model unbanned"})
}

// disableModel 手动禁用：禁用该模型全部组合（perm_banned，永不过期）。
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
	if err := breaker.New(s.store).BanAllModelKeys(m.ID, "manually disabled via admin API"); err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "model disabled"})
}

// modelKeyPair 解析并校验路径中的模型 id 与密钥 id（存在性），失败时已写入错误响应。
func (s *Server) modelKeyPair(w http.ResponseWriter, r *http.Request) (store.Model, store.ApiKey, bool) {
	modelID, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_id", "invalid model id")
		return store.Model{}, store.ApiKey{}, false
	}
	keyIDStr := chi.URLParam(r, "key_id")
	keyID, err := strconv.ParseInt(keyIDStr, 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_id", "invalid key id")
		return store.Model{}, store.ApiKey{}, false
	}
	var m store.Model
	if err := s.store.DB.First(&m, modelID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "model not found")
		} else {
			writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		}
		return m, store.ApiKey{}, false
	}
	var k store.ApiKey
	if err := s.store.DB.First(&k, keyID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "key not found")
		} else {
			writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		}
		return m, k, false
	}
	return m, k, true
}

// unbanModelKey 手动解禁模型-密钥组合。
func (s *Server) unbanModelKey(w http.ResponseWriter, r *http.Request) {
	m, k, ok := s.modelKeyPair(w, r)
	if !ok {
		return
	}
	rec := breaker.New(s.store)
	if err := rec.UnbanModelKey(m.ID, k.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "model-key combination unbanned"})
}

// banModelKey 手动禁用模型-密钥组合（perm_banned，永不过期；解禁走 DELETE bans/{key_id}）。
func (s *Server) banModelKey(w http.ResponseWriter, r *http.Request) {
	m, k, ok := s.modelKeyPair(w, r)
	if !ok {
		return
	}
	ban := store.ModelKeyBan{
		ModelID:   m.ID,
		KeyID:     k.ID,
		Status:    "perm_banned",
		BanReason: "manually disabled via admin API",
	}
	err := s.store.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "model_id"}, {Name: "key_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"status": "perm_banned", "banned_until": 0,
			"ban_reason": "manually disabled via admin API", "last_error": "",
		}),
	}).Create(&ban).Error
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "model-key combination banned"})
}

// modelKeyBanItem 某模型下单个密钥组合的禁用明细（轻量查询，不探测上游）。
type modelKeyBanItem struct {
	KeyID       int64  `json:"key_id"`
	KeyName     string `json:"key_name"`
	KeyMasked   string `json:"key_masked"`
	Status      string `json:"status"` // active | temp_banned | perm_banned
	BannedUntil int64  `json:"banned_until"`
	FailCount   int    `json:"fail_count"`
	BanReason   string `json:"ban_reason,omitempty"`
	LastError   string `json:"last_error,omitempty"`
}

// getModelKeyBans 返回模型下全部绑定密钥的组合禁用明细。
func (s *Server) getModelKeyBans(w http.ResponseWriter, r *http.Request) {
	modelID, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_id", "invalid model id")
		return
	}
	var m store.Model
	if err := s.store.DB.First(&m, modelID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "model not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	var mks []store.ModelKey
	if err := s.store.DB.Where("model_id = ?", modelID).Find(&mks).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	keyByID := map[int64]store.ApiKey{}
	if len(mks) > 0 {
		ids := make([]int64, 0, len(mks))
		for _, mk := range mks {
			ids = append(ids, mk.KeyID)
		}
		var keys []store.ApiKey
		if err := s.store.DB.Where("id IN ?", ids).Find(&keys).Error; err != nil {
			writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
			return
		}
		for _, k := range keys {
			keyByID[k.ID] = k
		}
	}
	banByKey := map[int64]store.ModelKeyBan{}
	var bans []store.ModelKeyBan
	if err := s.store.DB.Where("model_id = ?", modelID).Find(&bans).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	for _, b := range bans {
		banByKey[b.KeyID] = b
	}
	now := time.Now().Unix()
	items := make([]modelKeyBanItem, 0, len(mks))
	for _, mk := range mks {
		item := modelKeyBanItem{KeyID: mk.KeyID}
		if k, ok := keyByID[mk.KeyID]; ok {
			item.KeyName = k.Name
			item.KeyMasked = maskKey(k.KeyValue)
		}
		if b, ok := banByKey[mk.KeyID]; ok {
			item.FailCount = b.FailCount
			item.LastError = b.LastError
			switch {
			case b.Status == "perm_banned":
				item.Status = "perm_banned"
				item.BanReason = b.BanReason
			case b.Status == "temp_banned" && b.BannedUntil > now:
				item.Status = "temp_banned"
				item.BannedUntil = b.BannedUntil
				item.BanReason = b.BanReason
			default:
				item.Status = "active" // 过期 temp_banned 半开可用
			}
		} else {
			item.Status = "active"
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, items)
}
