package api

import (
	"net/http"

	"github.com/cloudomni/omnigate/internal/store"
)

// postMaintenanceCleanup 立即按当前保留期配置执行一次清理（与后台每小时定时清理同一逻辑）。
func (s *Server) postMaintenanceCleanup(w http.ResponseWriter, _ *http.Request) {
	rt := s.rt.Snapshot()
	deleted, err := store.PurgeRetentions(s.store.DB, rt.LogRetentionDays, rt.CaptureRetentionDays)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

// postMaintenanceClearLogs 清空请求明细（request_log / attempt），保留每日统计与内容日志。
// 危险操作：要求 body {"confirm":true} 作为二次确认。
func (s *Server) postMaintenanceClearLogs(w http.ResponseWriter, r *http.Request) {
	if !requireConfirm(w, r) {
		return
	}
	cleared, err := store.ClearLogs(s.store.DB)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": cleared})
}

// postMaintenanceClearStats 清空全部统计（请求日志 / 尝试明细 / 日聚合）。
// 危险操作：要求 body {"confirm":true} 作为二次确认，content_log 不受影响。
func (s *Server) postMaintenanceClearStats(w http.ResponseWriter, r *http.Request) {
	if !requireConfirm(w, r) {
		return
	}
	cleared, err := store.ClearStats(s.store.DB)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": cleared})
}

// requireConfirm 解析 body 并要求 {"confirm":true}；不满足时写出 400 并返回 false。
func requireConfirm(w http.ResponseWriter, r *http.Request) bool {
	var req struct {
		Confirm bool `json:"confirm"`
	}
	if err := decodeBody(r, &req); err != nil || !req.Confirm {
		writeErr(w, http.StatusBadRequest, "confirm_required", `body must be {"confirm":true}`)
		return false
	}
	return true
}
