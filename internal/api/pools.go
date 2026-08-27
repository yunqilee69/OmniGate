package api

import (
	"errors"
	"net/http"
	"strings"

	"gorm.io/gorm"

	"github.com/cloudomni/omnigate/internal/store"
)

// keyResp ApiKey 的对外展示：内嵌实体（KeyValue 带 json:"-"）+ 脱敏值。
type keyResp struct {
	store.ApiKey
	KeyValueMasked string `json:"key_value"`
}

type poolResp struct {
	store.KeyPool
	Keys []keyResp `json:"keys"`
}

type poolCreateReq struct {
	ProviderID int64  `json:"provider_id"`
	Name       string `json:"name"`
	Weight     int    `json:"weight"`
	Remark     string `json:"remark"`
}

func (s *Server) listPools(w http.ResponseWriter, _ *http.Request) {
	var pools []store.KeyPool
	if err := s.store.DB.Order("id").Find(&pools).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	var keys []store.ApiKey
	if err := s.store.DB.Order("id").Find(&keys).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	byPool := map[int64][]keyResp{}
	for _, k := range keys {
		byPool[k.PoolID] = append(byPool[k.PoolID], keyResp{ApiKey: k, KeyValueMasked: maskKey(k.KeyValue)})
	}
	out := make([]poolResp, 0, len(pools))
	for _, p := range pools {
		ks := byPool[p.ID]
		if ks == nil {
			ks = []keyResp{}
		}
		out = append(out, poolResp{KeyPool: p, Keys: ks})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createPool(w http.ResponseWriter, r *http.Request) {
	var req poolCreateReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.ProviderID <= 0 || req.Name == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "provider_id and name are required")
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
	if req.Weight <= 0 {
		req.Weight = 1
	}
	p := store.KeyPool{ProviderID: req.ProviderID, Name: req.Name, Weight: req.Weight, Remark: req.Remark}
	if err := s.store.DB.Create(&p).Error; err != nil {
		if isUniqueErr(err) {
			writeErr(w, http.StatusConflict, "conflict", "pool name already exists under this provider")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, poolResp{KeyPool: p, Keys: []keyResp{}})
}

type poolUpdateReq struct {
	ProviderID *int64  `json:"provider_id"`
	Name       *string `json:"name"`
	Weight     *int    `json:"weight"`
	Remark     *string `json:"remark"`
}

func (s *Server) updatePool(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	var req poolUpdateReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	var p store.KeyPool
	if err := s.store.DB.First(&p, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "pool not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if req.ProviderID != nil && *req.ProviderID != p.ProviderID {
		writeErr(w, http.StatusBadRequest, "bad_request", "changing pool provider is not allowed; delete and recreate the pool")
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
	if req.Weight != nil {
		if *req.Weight <= 0 {
			writeErr(w, http.StatusBadRequest, "bad_request", "weight must be positive")
			return
		}
		updates["weight"] = *req.Weight
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
			writeErr(w, http.StatusConflict, "conflict", "pool name already exists under this provider")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	_ = s.store.DB.First(&p, id).Error
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) deletePool(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	var p store.KeyPool
	if err := s.store.DB.First(&p, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "pool not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	err := s.store.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("pool_id = ?", id).Delete(&store.ApiKey{}).Error; err != nil {
			return err
		}
		if err := tx.Where("pool_id = ?", id).Delete(&store.ModelPool{}).Error; err != nil {
			return err
		}
		return tx.Delete(&store.KeyPool{}, id).Error
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}
