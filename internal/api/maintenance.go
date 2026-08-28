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

// postMaintenanceClearStats 清空全部统计（请求日志 / 尝试明细 / 日聚合）。
// 危险操作：要求 body {"confirm":true} 作为二次确认，content_log 不受影响。
func (s *Server) postMaintenanceClearStats(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Confirm bool `json:"confirm"`
	}
	if err := decodeBody(r, &req); err != nil || !req.Confirm {
		writeErr(w, http.StatusBadRequest, "confirm_required", `body must be {"confirm":true}`)
		return
	}
	cleared, err := store.ClearStats(s.store.DB)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": cleared})
}
