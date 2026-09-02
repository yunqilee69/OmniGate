package api

import (
	"errors"
	"net/http"
	"strings"

	"gorm.io/gorm"

	"github.com/cloudomni/omnigate/internal/store"
)

// routeTargetResp 路由目标的对外展示（带模型与提供商名称，供 UI 直接渲染）。
type routeTargetResp struct {
	ID           int64  `json:"id"`
	ModelID      int64  `json:"model_id"`
	ModelName    string `json:"model_name"`
	ProviderID   int64  `json:"provider_id"`
	ProviderName string `json:"provider_name"`
	Weight       int    `json:"weight"`
}

// routeResp 内嵌 store.Route；外层 Targets 字段遮蔽内层同名 json 键，输出富化后的目标。
type routeResp struct {
	store.Route
	Targets []routeTargetResp `json:"targets"`
}

type routeTargetReq struct {
	ModelID int64 `json:"model_id"`
	Weight  int   `json:"weight"`
}

type routeCreateReq struct {
	Name     string           `json:"name"`
	Endpoint string           `json:"endpoint"`
	Remark   string           `json:"remark"`
	Targets  []routeTargetReq `json:"targets"`
}

func (s *Server) enrichTargets(targets []store.RouteTarget) []routeTargetResp {
	var models []store.Model
	_ = s.store.DB.Find(&models).Error
	var providers []store.Provider
	_ = s.store.DB.Find(&providers).Error
	mInfo := map[int64]store.Model{}
	for _, m := range models {
		mInfo[m.ID] = m
	}
	pInfo := map[int64]store.Provider{}
	for _, p := range providers {
		pInfo[p.ID] = p
	}
	out := make([]routeTargetResp, 0, len(targets))
	for _, t := range targets {
		tr := routeTargetResp{ID: t.ID, ModelID: t.ModelID, Weight: t.Weight}
		if m, ok := mInfo[t.ModelID]; ok {
			tr.ModelName = m.Name
			tr.ProviderID = m.ProviderID
			if p, ok2 := pInfo[m.ProviderID]; ok2 {
				tr.ProviderName = p.Name
			}
		}
		out = append(out, tr)
	}
	return out
}

func (s *Server) listRoutes(w http.ResponseWriter, _ *http.Request) {
	var routes []store.Route
	if err := s.store.DB.Preload("Targets").Order("id").Find(&routes).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	out := make([]routeResp, 0, len(routes))
	for _, rt := range routes {
		out = append(out, routeResp{Route: rt, Targets: s.enrichTargets(rt.Targets)})
	}
	writeJSON(w, http.StatusOK, out)
}

// validateTargets 校验目标模型存在、权重合法、无重复模型、协议匹配endpoint。
func (s *Server) validateTargets(endpoint string, targets []routeTargetReq) (bool, string) {
	seen := map[int64]bool{}
	var ids []int64
	for i := range targets {
		t := &targets[i]
		if t.ModelID <= 0 {
			return false, "target model_id must be positive"
		}
		if seen[t.ModelID] {
			return false, "duplicate model in targets"
		}
		seen[t.ModelID] = true
		if t.Weight <= 0 {
			t.Weight = 1
		}
		ids = append(ids, t.ModelID)
	}
	if len(ids) == 0 {
		return true, ""
	}
	var models []store.Model
	if err := s.store.DB.Where("id IN ?", ids).Find(&models).Error; err != nil {
		return false, err.Error()
	}
	if len(models) != len(ids) {
		return false, "部分目标模型不存在"
	}
	
	expectedProtocol := endpointToProtocol(endpoint)
	for _, m := range models {
		if m.Protocol != expectedProtocol {
			return false, "模型 '" + m.Name + "' 使用协议 '" + m.Protocol + "'，但路由端点 '" + endpoint + "' 需要协议 '" + expectedProtocol + "'"
		}
	}
	return true, ""
}

func endpointToProtocol(endpoint string) string {
	switch endpoint {
	case "messages":
		return "anthropic"
	case "responses":
		return "responses"
	default:
		return "openai"
	}
}

func (s *Server) createRoute(w http.ResponseWriter, r *http.Request) {
	var req routeCreateReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	req.Endpoint = strings.TrimSpace(req.Endpoint)
	if req.Endpoint == "" {
		req.Endpoint = "chat"
	}
	if req.Endpoint != "chat" && req.Endpoint != "messages" && req.Endpoint != "responses" {
		writeErr(w, http.StatusBadRequest, "bad_request", "endpoint must be one of: chat, messages, responses")
		return
	}
	if ok, msg := s.validateTargets(req.Endpoint, req.Targets); !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", msg)
		return
	}
	rt := store.Route{Name: req.Name, Endpoint: req.Endpoint, Remark: req.Remark}
	err := s.store.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&rt).Error; err != nil {
			return err
		}
		for _, t := range req.Targets {
			if err := tx.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: t.ModelID, Weight: t.Weight}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if isUniqueErr(err) {
			writeErr(w, http.StatusConflict, "conflict", "route name already exists")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	var targets []store.RouteTarget
	_ = s.store.DB.Where("route_id = ?", rt.ID).Order("id").Find(&targets).Error
	writeJSON(w, http.StatusCreated, routeResp{Route: rt, Targets: s.enrichTargets(targets)})
}

type routeUpdateReq struct {
	Name     *string          `json:"name"`
	Endpoint *string          `json:"endpoint"`
	Remark   *string          `json:"remark"`
	Targets  []routeTargetReq `json:"targets"`
}

func (s *Server) updateRoute(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	var req routeUpdateReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	var rt store.Route
	if err := s.store.DB.First(&rt, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "route not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
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
	if req.Endpoint != nil {
		v := strings.TrimSpace(*req.Endpoint)
		if v != "chat" && v != "messages" && v != "responses" {
			writeErr(w, http.StatusBadRequest, "bad_request", "endpoint must be one of: chat, messages, responses")
			return
		}
		simple["endpoint"] = v
	}
	if req.Remark != nil {
		simple["remark"] = *req.Remark
	}
	if len(simple) == 0 && req.Targets == nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "no fields to update")
		return
	}
	
	effectiveEndpoint := rt.Endpoint
	if req.Endpoint != nil {
		effectiveEndpoint = strings.TrimSpace(*req.Endpoint)
	}
	if req.Targets != nil {
		if ok, msg := s.validateTargets(effectiveEndpoint, req.Targets); !ok {
			writeErr(w, http.StatusBadRequest, "bad_request", msg)
			return
		}
	}
	err := s.store.DB.Transaction(func(tx *gorm.DB) error {
		if len(simple) > 0 {
			if err := tx.Model(&rt).Updates(simple).Error; err != nil {
				return err
			}
		}
		if req.Targets != nil {
			if err := tx.Where("route_id = ?", id).Delete(&store.RouteTarget{}).Error; err != nil {
				return err
			}
			for _, t := range req.Targets {
				if err := tx.Create(&store.RouteTarget{RouteID: id, ModelID: t.ModelID, Weight: t.Weight}).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		if isUniqueErr(err) {
			writeErr(w, http.StatusConflict, "conflict", "route name already exists")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	_ = s.store.DB.Preload("Targets").First(&rt, id).Error
	writeJSON(w, http.StatusOK, routeResp{Route: rt, Targets: s.enrichTargets(rt.Targets)})
}

func (s *Server) deleteRoute(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	var rt store.Route
	if err := s.store.DB.First(&rt, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "route not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	err := s.store.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("route_id = ?", id).Delete(&store.RouteTarget{}).Error; err != nil {
			return err
		}
		return tx.Delete(&store.Route{}, id).Error
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// v1Models 代理面模型列表：返回全部逻辑路由名（OpenAI 兼容格式，无需鉴权）。
func (s *Server) v1Models(w http.ResponseWriter, _ *http.Request) {
	var routes []store.Route
	if err := s.store.DB.Order("id").Find(&routes).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	data := make([]map[string]string, 0, len(routes))
	for _, rt := range routes {
		data = append(data, map[string]string{"id": rt.Name, "object": "model", "owned_by": "omnigate"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
