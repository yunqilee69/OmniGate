package api

import (
	"errors"
	"net/http"
	"strings"

	"gorm.io/gorm"

	"github.com/cloudomni/omnigate/internal/store"
)

type keyListResp struct {
	keyResp
	PoolName string `json:"pool_name"`
}

type keyCreateReq struct {
	PoolID   int64  `json:"pool_id"`
	Keys     string `json:"keys"`      // 批量导入：换行分隔
	KeyValue string `json:"key_value"` // 单个导入
	Name     string `json:"name"`
}

func (s *Server) listKeys(w http.ResponseWriter, _ *http.Request) {
	var keys []store.ApiKey
	if err := s.store.DB.Order("id").Find(&keys).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	var pools []store.KeyPool
	if err := s.store.DB.Find(&pools).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	poolName := map[int64]string{}
	for _, p := range pools {
		poolName[p.ID] = p.Name
	}
	out := make([]keyListResp, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyListResp{
			keyResp:  keyResp{ApiKey: k, KeyValueMasked: maskKey(k.KeyValue)},
			PoolName: poolName[k.PoolID],
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// createKeys 支持单发（key_value）与批量（keys，换行分隔）导入；请求内与库内重复值自动跳过。
func (s *Server) createKeys(w http.ResponseWriter, r *http.Request) {
	var req keyCreateReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.PoolID <= 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "pool_id is required")
		return
	}
	var pool store.KeyPool
	if err := s.store.DB.First(&pool, req.PoolID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "pool not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	var values []string
	seen := map[string]bool{}
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			values = append(values, v)
		}
	}
	for _, line := range strings.Split(req.Keys, "\n") {
		add(line)
	}
	add(req.KeyValue)
	if len(values) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "no keys provided (use key_value or newline-separated keys)")
		return
	}
	var existing []store.ApiKey
	if err := s.store.DB.Where("pool_id = ? AND key_value IN ?", req.PoolID, values).Find(&existing).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	dup := map[string]bool{}
	for _, e := range existing {
		dup[e.KeyValue] = true
	}
	var created []store.ApiKey
	err := s.store.DB.Transaction(func(tx *gorm.DB) error {
		for _, v := range values {
			if dup[v] {
				continue
			}
			k := store.ApiKey{PoolID: req.PoolID, KeyValue: v, Name: req.Name, Status: "active"}
			if err := tx.Create(&k).Error; err != nil {
				return err
			}
			created = append(created, k)
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	resp := make([]keyResp, len(created))
	for i, k := range created {
		resp[i] = keyResp{ApiKey: k, KeyValueMasked: maskKey(k.KeyValue)}
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"created":            len(resp),
		"skipped_duplicates": len(values) - len(resp),
		"keys":               resp,
	})
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
		updates["key_value"] = v
	}
	if req.Name != nil {
		updates["name"] = *req.Name
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
	res := s.store.DB.Delete(&store.ApiKey{}, id)
	if res.Error != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", res.Error.Error())
		return
	}
	if res.RowsAffected == 0 {
		writeErr(w, http.StatusNotFound, "not_found", "key not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}
