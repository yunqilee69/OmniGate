package api

import (
	"net/http"
	"strings"

	"github.com/cloudomni/omnigate/internal/proxy"
	"github.com/cloudomni/omnigate/internal/router"
)

func (s *Server) testModel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	writeJSON(w, http.StatusOK, proxy.ProbeModel(s.store, s.rt, id))
}

func (s *Server) testModelKeys(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	result, found := proxy.ProbeModelKeys(s.store, s.rt, id)
	if !found {
		writeErr(w, http.StatusNotFound, "not_found", "model not found")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type testModelByNameReq struct {
	Name string `json:"name"`
}

// testModelKeysByName 按 provider/model 直达测试：定位物理模型后逐密钥探测。
// 名字无第一个 / 或为空 → 400；提供商或模型不存在 → 404。
func (s *Server) testModelKeysByName(w http.ResponseWriter, r *http.Request) {
	var req testModelByNameReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "invalid body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if _, _, ok := router.SplitProviderModel(req.Name); !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "expected provider/model")
		return
	}
	result, found := proxy.ProbeModelKeysByName(s.store, s.rt, req.Name)
	if !found {
		writeErr(w, http.StatusNotFound, "not_found", "model not found")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) testProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	results, found := proxy.ProbeProvider(s.store, s.rt, id)
	if !found {
		writeErr(w, http.StatusNotFound, "not_found", "provider not found")
		return
	}
	writeJSON(w, http.StatusOK, results)
}
