package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cloudomni/omnigate/internal/router"
	"github.com/cloudomni/omnigate/internal/store"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"
)

// 视频生成为异步任务家族：POST /v1/videos 提交、GET /v1/videos/{id} 轮询、
// GET /v1/videos/{id}/content 下载。上游格式无跨厂商标准（Sora 用 /videos、
// Runway 用 /image_to_video，参数集互不兼容），因此与 embeddings/images 同方针：
// 请求体直通、仅重写 model 字段，不做协议转换。
//
// 提交走 serveTyped（复用类型过滤、密钥轮询、熔断、重试、日志与 VK 结算）；
// 轮询与下载不是模型调用而是任务回查，无法用 model 字段定位上游，故由 video_task
// 表把上游返回的 task id 映射回提交时命中的 提供商×模型×密钥。

var videoKind = typedKind{
	modelType: "video",
	path:      "/videos",
	parseUsage: func(body []byte) usageInfo {
		// 视频生成通常按次计费（billing_mode=per_call）；上游若回显时长则记入 seconds，
		// 供 audio_second 计费模式与统计使用。Sora 把 seconds 写成字符串（"8"），
		// 部分厂商写数字，两种形态都要吃下。
		var parsed struct {
			Seconds json.RawMessage `json:"seconds"`
		}
		if json.Unmarshal(body, &parsed) != nil || len(parsed.Seconds) == 0 {
			return usageInfo{}
		}
		var f float64
		if err := json.Unmarshal(parsed.Seconds, &f); err != nil {
			var s string
			if json.Unmarshal(parsed.Seconds, &s) != nil {
				return usageInfo{}
			}
			n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			if err != nil {
				return usageInfo{}
			}
			f = n
		}
		if f <= 0 {
			return usageInfo{}
		}
		return usageInfo{seconds: f}
	},
	onSuccess: registerVideoTask,
}

// Videos 处理 POST /v1/videos（异步提交，上游返回 task id）。
func (h *Handler) Videos(w http.ResponseWriter, r *http.Request) {
	h.serveTyped(w, r, videoKind)
}

// registerVideoTask 把上游 task id 与命中落点（提供商/模型/密钥）绑定入库，
// 供后续轮询与下载回查。上游未返回 id（同步结果）时无需登记。
func registerVideoTask(h *Handler, att router.Attempt, route string, respBody []byte) {
	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil || parsed.ID == "" {
		return
	}
	updates := map[string]any{
		"route":       route,
		"model_id":    att.Model.ID,
		"provider_id": att.Provider.ID,
		"key_id":      att.Key.ID,
	}
	// video_id 唯一：命中已有行则覆盖落点（以最后一次成功提交为准），不改 created_at。
	res := h.db.DB.Where("video_id = ?", parsed.ID).Assign(updates).FirstOrCreate(&store.VideoTask{VideoID: parsed.ID})
	if res.Error != nil {
		slog.Error("record video task failed", "err", res.Error, "video_id", parsed.ID, "route", route)
	}
}

// GetVideo 处理 GET /v1/videos/{id}（轮询任务状态，直通上游 JSON）。
func (h *Handler) GetVideo(w http.ResponseWriter, r *http.Request) {
	h.serveVideoTask(w, r, "")
}

// DownloadVideo 处理 GET /v1/videos/{id}/content（下载成片，流式透传字节）。
func (h *Handler) DownloadVideo(w http.ResponseWriter, r *http.Request) {
	h.serveVideoTask(w, r, "/content")
}

// serveVideoTask 按 task id 回查上游：video_task 命中后原样转发 GET，响应体直通
// （轮询为 JSON、下载为视频字节流，网关都不解析）。suffix 为上游路径后缀。
func (h *Handler) serveVideoTask(w http.ResponseWriter, r *http.Request, suffix string) {
	requestID := newRequestID()
	w.Header().Set("X-Request-Id", requestID)

	videoID := chi.URLParam(r, "id")
	if videoID == "" {
		openAIError(w, http.StatusBadRequest, "invalid_request", "video id is required", nil)
		return
	}

	var task store.VideoTask
	if err := h.db.DB.Where("video_id = ?", videoID).First(&task).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 只认本地登记过的任务：否则任意 id 都会被带上提供商密钥转发到上游，
			// 网关退化成开放代理。
			openAIError(w, http.StatusNotFound, "video_not_found",
				"video '"+videoID+"' was not created through this gateway", nil)
			return
		}
		openAIError(w, http.StatusInternalServerError, "db_error", "failed to look up video task", nil)
		return
	}

	var provider store.Provider
	if err := h.db.DB.First(&provider, task.ProviderID).Error; err != nil {
		openAIError(w, http.StatusNotFound, "provider_not_found",
			"provider for this video task no longer exists", nil)
		return
	}
	var key store.ApiKey
	if err := h.db.DB.First(&key, task.KeyID).Error; err != nil {
		openAIError(w, http.StatusNotFound, "key_not_found",
			"api key for this video task no longer exists", nil)
		return
	}

	upURL := strings.TrimRight(provider.BaseURL, "/") + "/videos/" + url.PathEscape(videoID) + suffix
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upURL, nil)
	if err != nil {
		openAIError(w, http.StatusInternalServerError, errBadUpstreamURL, "failed to build upstream request", nil)
		return
	}
	upReq.Header.Set("Authorization", "Bearer "+key.KeyValue)
	ApplyUpstreamIdentity(upReq, provider, r)

	resp, err := h.clientForProvider(provider.ID).Do(upReq)
	if err != nil {
		slog.Error("video task upstream failed", "err", err, "video_id", videoID, "provider", provider.Name)
		openAIError(w, http.StatusBadGateway, errConnectionFailed, "failed to reach upstream for video task", nil)
		return
	}
	defer resp.Body.Close()

	// 透传内容协商相关头（下载场景 Content-Type/Length 由上游决定）；
	// 逐跳头与已由网关掌控的 X-Request-Id 不带过。
	copyTaskResponseHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		slog.Warn("video task body forward broken", "err", err, "video_id", videoID)
	}
}

// copyTaskResponseHeaders 透传上游响应头，剔除逐跳头与网关自管的字段。
func copyTaskResponseHeaders(w http.ResponseWriter, src http.Header) {
	dropHopByHop := map[string]bool{
		"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
		"Proxy-Authorization": true, "Te": true, "Trailer": true, "Transfer-Encoding": true,
		"Upgrade": true, "Content-Encoding": true, "X-Request-Id": true,
	}
	for k, vs := range src {
		if dropHopByHop[k] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
}
