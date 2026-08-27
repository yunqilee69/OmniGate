package api

import (
	"errors"
	"net/http"
	"strings"

	"gorm.io/gorm"

	"github.com/cloudomni/omnigate/internal/store"
)

// keyResp ApiKey 的对外展示：内嵌实体（KeyValue 带 json:"-"）+ 脱敏值；reveal=1 时附带明文
//（本地单人场景，明文仅供配置页“显示密钥”按钮使用）。
type keyResp struct {
	store.ApiKey
	KeyValueMasked string  `json:"key_value"`
	KeyValuePlain  *string `json:"key_value_plain,omitempty"`
}

type keyCreateReq struct {
	ProviderID int64  `json:"provider_id"`
	KeyValue   string `json:"key_value"`
	Name       string `json:"name"`
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	var keys []store.ApiKey
	if err := s.store.DB.Order("id").Find(&keys).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	reveal := r.URL.Query().Get("reveal") == "1"
	out := make([]keyResp, 0, len(keys))
	for _, k := range keys {
		item := keyResp{ApiKey: k, KeyValueMasked: maskKey(k.KeyValue)}
		if reveal {
			plain := k.KeyValue
			item.KeyValuePlain = &plain
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

// createKeys 新增单个密钥：名称必填且同提供商内唯一；密钥值重复返回 409。
func (s *Server) createKeys(w http.ResponseWriter, r *http.Request) {
	var req keyCreateReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.ProviderID <= 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "provider_id is required")
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
	name := strings.TrimSpace(req.Name)
	value := strings.TrimSpace(req.KeyValue)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	if value == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "key_value is required")
		return
	}
	var cnt int64
	if err := s.store.DB.Model(&store.ApiKey{}).
		Where("provider_id = ? AND name = ?", req.ProviderID, name).Count(&cnt).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if cnt > 0 {
		writeErr(w, http.StatusConflict, "conflict", "key name already exists in this provider")
		return
	}
	if err := s.store.DB.Model(&store.ApiKey{}).
		Where("provider_id = ? AND key_value = ?", req.ProviderID, value).Count(&cnt).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if cnt > 0 {
		writeErr(w, http.StatusConflict, "conflict", "key value already exists in this provider")
		return
	}
	k := store.ApiKey{ProviderID: req.ProviderID, KeyValue: value, Name: name, Status: "active"}
	if err := s.store.DB.Create(&k).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, keyResp{ApiKey: k, KeyValueMasked: maskKey(k.KeyValue)})
}

type keyUpdateReq struct {
	KeyValue *string `json:"key_value"`
	Name     *string `json:"name"`
	Status   *string `json:"status"`
}

func (s *Server) updateKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	var req keyUpdateReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	var k store.ApiKey
	if err := s.store.DB.First(&k, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "key not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	updates := map[string]any{}
	if req.KeyValue != nil && !strings.Contains(*req.KeyValue, "****") { // 脱敏值回传视为不修改
		v := strings.TrimSpace(*req.KeyValue)
		if v == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "key_value must not be empty")
			return
		}
		var cnt int64
		if err := s.store.DB.Model(&store.ApiKey{}).
			Where("provider_id = ? AND key_value = ? AND id <> ?", k.ProviderID, v, k.ID).
			Count(&cnt).Error; err != nil {
			writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
			return
		}
		if cnt > 0 {
			writeErr(w, http.StatusConflict, "conflict", "key value already exists in this provider")
			return
		}
		updates["key_value"] = v
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "name must not be empty")
			return
		}
		var cnt int64
		if err := s.store.DB.Model(&store.ApiKey{}).
			Where("provider_id = ? AND name = ? AND id <> ?", k.ProviderID, name, k.ID).
			Count(&cnt).Error; err != nil {
			writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
			return
		}
		if cnt > 0 {
			writeErr(w, http.StatusConflict, "conflict", "key name already exists in this provider")
			return
		}
		updates["name"] = name
	}
	if req.Status != nil {
		switch *req.Status {
		case "active": // 手动启用：清空冷却与禁用痕迹
			updates["status"] = "active"
			updates["cooldown_until"] = 0
			updates["disable_reason"] = ""
		case "disabled":
			updates["status"] = "disabled"
			updates["disable_reason"] = "manually disabled via admin API"
		default:
			writeErr(w, http.StatusBadRequest, "bad_request", "status must be active or disabled")
			return
		}
	}
	if len(updates) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "no fields to update")
		return
	}
	if err := s.store.DB.Model(&k).Updates(updates).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	_ = s.store.DB.First(&k, id).Error
	writeJSON(w, http.StatusOK, keyResp{ApiKey: k, KeyValueMasked: maskKey(k.KeyValue)})
}

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	err := s.store.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("key_id = ?", id).Delete(&store.ModelKey{}).Error; err != nil {
			return err
		}
		res := tx.Delete(&store.ApiKey{}, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "key not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}
