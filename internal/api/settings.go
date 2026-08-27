package api

import (
	"encoding/json"
	"net/http"
)

func (s *Server) getSettings(w http.ResponseWriter, _ *http.Request) {
	all, err := s.rt.All()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, all)
}

// putSettings 部分更新运行层配置；校验通过后写入 DB 并原子重建快照（热生效）。
func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var payload map[string]json.RawMessage
	if err := decodeBody(r, &payload); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := s.rt.Update(payload); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_settings", err.Error())
		return
	}
	all, err := s.rt.All()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, all)
}
