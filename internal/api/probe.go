package api

import (
	"net/http"

	"github.com/cloudomni/omnigate/internal/proxy"
)

func (s *Server) testModel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	writeJSON(w, http.StatusOK, proxy.ProbeModel(s.store, s.rt, id))
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
