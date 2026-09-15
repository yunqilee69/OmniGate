package proxy_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cloudomni/omnigate/internal/store"
)

// seedVideoModel 建一个 video 类型后端 + 路由 "vid"，返回上游收到的最后一跳信息。
func seedVideoModel(t *testing.T, st *store.Store, baseURL string) (route string) {
	t.Helper()
	p := store.Provider{Name: "vidprov", BaseURL: baseURL}
	if err := st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	m := store.Model{ProviderID: p.ID, Name: "sora-2", Type: "video",
		BillingMode: "per_call", PerCallPrice: 4} // per_call 单价即每次调用的绝对价格
	if err := st.DB.Create(&m).Error; err != nil {
		t.Fatal(err)
	}
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-v-1", Name: "k1", Status: "active"}
	if err := st.DB.Create(&k).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID}).Error; err != nil {
		t.Fatal(err)
	}
	rt := store.Route{Name: "vid", Endpoint: "video"}
	if err := st.DB.Create(&rt).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1}).Error; err != nil {
		t.Fatal(err)
	}
	return "vid"
}

// TestVideoSubmitRoutesAndLogs 提交：路由到 video 后端、透传上游任务对象、
// 落 request_log（endpoint=video、per_call 计费）并登记 video_task 映射。
func TestVideoSubmitRoutesAndLogs(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)

	var gotPath, gotAuth, gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		gotModel, _ = m["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"video_abc","object":"video","status":"queued","model":"sora-2","seconds":"8"}`)
	}))
	defer up.Close()
	seedVideoModel(t, st, up.URL+"/v1")

	resp := typedPost(t, h, "/v1/videos", map[string]any{
		"model": "vid", "prompt": "a cool cat", "size": "1280x720",
	}, vkToken)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d — %s", resp.StatusCode, readAll(t, resp))
	}
	if gotPath != "/v1/videos" {
		t.Errorf("upstream path = %s, want /v1/videos", gotPath)
	}
	if gotAuth != "Bearer sk-v-1" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotModel != "sora-2" {
		t.Errorf("upstream model = %q, want physical name sora-2", gotModel)
	}

	lg := logs(t, st)
	if len(lg) != 1 {
		t.Fatalf("logs = %d, want 1", len(lg))
	}
	if lg[0].Endpoint != "video" || lg[0].Status != "success" {
		t.Errorf("log mismatch: %+v", lg[0])
	}
	if lg[0].Cost != 4 { // per_call 4 USD
		t.Errorf("cost = %v, want 4", lg[0].Cost)
	}
	if lg[0].AudioSeconds != 8 {
		t.Errorf("audio_seconds = %v, want 8 (string seconds must parse)", lg[0].AudioSeconds)
	}

	var task store.VideoTask
	if err := st.DB.Where("video_id = ?", "video_abc").First(&task).Error; err != nil {
		t.Fatalf("video_task not registered: %v", err)
	}
	if task.Route != "vid" || task.ProviderID == 0 || task.KeyID == 0 {
		t.Errorf("task mapping incomplete: %+v", task)
	}
}

// TestVideoTaskPollAndDownloadRoundTrip 轮询与下载必须回到提交时命中的上游与密钥。
func TestVideoTaskPollAndDownloadRoundTrip(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)

	var hits []string
	var auths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		auths = append(auths, r.Header.Get("Authorization"))
		if r.URL.Path == "/v1/videos/video_xyz/content" {
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("MP4BYTES"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"video_xyz","status":"completed"}`)
	}))
	defer up.Close()
	seedVideoModel(t, st, up.URL+"/v1")

	// 提交以建立 video_task 映射
	if resp := typedPost(t, h, "/v1/videos", map[string]any{
		"model": "vid", "prompt": "x",
	}, vkToken); resp.StatusCode != 200 {
		t.Fatalf("submit: %d — %s", resp.StatusCode, readAll(t, resp))
	}

	do := func(path string) (*http.Response, []byte) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+vkToken)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Result(), rec.Body.Bytes()
	}

	statusResp, statusBody := do("/v1/videos/video_xyz")
	if statusResp.StatusCode != 200 {
		t.Fatalf("poll status = %d — %s", statusResp.StatusCode, statusBody)
	}
	if !bytes.Contains(statusBody, []byte(`"status":"completed"`)) {
		t.Errorf("poll body = %s", statusBody)
	}

	dlResp, dlBody := do("/v1/videos/video_xyz/content")
	if dlResp.StatusCode != 200 {
		t.Fatalf("download status = %d — %s", dlResp.StatusCode, dlBody)
	}
	if string(dlBody) != "MP4BYTES" {
		t.Errorf("download body = %q", dlBody)
	}
	if ct := dlResp.Header.Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("download content-type = %q, want upstream value passthrough", ct)
	}

	want := []string{"/v1/videos", "/v1/videos/video_xyz", "/v1/videos/video_xyz/content"}
	if len(hits) != len(want) {
		t.Fatalf("upstream hits = %v, want %v", hits, want)
	}
	for i := range want {
		if hits[i] != want[i] {
			t.Errorf("hit %d = %s, want %s", i, hits[i], want[i])
		}
		if auths[i] != "Bearer sk-v-1" {
			t.Errorf("hit %d auth = %q", i, auths[i])
		}
	}
}

// TestVideoTaskUnknownIDRejected 网关必须只回查自己登记过的任务：
// 未知 id 若被转发，网关就成了带提供商密钥的开放代理。
func TestVideoTaskUnknownIDRejected(t *testing.T) {
	_, h, vkToken := newTestStackWithVK(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/videos/not_mine", nil)
	req.Header.Set("Authorization", "Bearer "+vkToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d — %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("video_not_found")) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

// TestVideoUpstreamErrorPassthrough 上游 4xx 原样透传，并记为 client_error。
func TestVideoUpstreamErrorPassthrough(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"input images with faces are rejected"}}`)
	}))
	defer up.Close()
	seedVideoModel(t, st, up.URL+"/v1")

	resp := typedPost(t, h, "/v1/videos", map[string]any{"model": "vid", "prompt": "x"}, vkToken)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 passthrough", resp.StatusCode)
	}
	body := readAll(t, resp)
	if !bytes.Contains([]byte(body), []byte("faces")) {
		t.Errorf("upstream error body must pass through, got %s", body)
	}

	lg := logs(t, st)
	if len(lg) != 1 || lg[0].Status != "client_error" {
		t.Fatalf("log mismatch: %+v", lg)
	}
	if lg[0].Cost != 0 {
		t.Errorf("failed submit must not bill, cost = %v", lg[0].Cost)
	}
	var n int64
	st.DB.Model(&store.VideoTask{}).Count(&n)
	if n != 0 {
		t.Errorf("failed submit must not register task, got %d", n)
	}
}

// TestVideoSubmitRejectsNonVideoBackends chat 后端不得承接视频提交。
func TestVideoSubmitRejectsNonVideoBackends(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("chat backend must not be selected for video: %s", r.URL.Path)
		w.WriteHeader(500)
	}))
	defer up.Close()

	p := store.Provider{Name: "p", BaseURL: up.URL + "/v1"}
	st.DB.Create(&p)
	m := store.Model{ProviderID: p.ID, Name: "glm-4.6", Type: "chat"}
	st.DB.Create(&m)
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-c", Name: "c", Status: "active"}
	st.DB.Create(&k)
	st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID})
	rt := store.Route{Name: "vid", Endpoint: "video"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})

	resp := typedPost(t, h, "/v1/videos", map[string]any{"model": "vid", "prompt": "x"}, vkToken)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d — %s", resp.StatusCode, readAll(t, resp))
	}
	if body := readAll(t, resp); !bytes.Contains([]byte(body), []byte("all_backends_unavailable")) {
		t.Errorf("body = %s", body)
	}
}

// TestVideoMissingFields 缺 model → 400；prompt 不做网关侧校验（与 images 同方针，
// 直通转发由上游拒绝），但未知路由必须是 404 而不是被当成提示词错误。
func TestVideoMissingFields(t *testing.T) {
	_, h, vkToken := newTestStackWithVK(t)

	resp := typedPost(t, h, "/v1/videos", map[string]any{"prompt": "x"}, vkToken)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing model: status = %d — %s", resp.StatusCode, readAll(t, resp))
	}

	resp = typedPost(t, h, "/v1/videos", map[string]any{"model": "no-such-route"}, vkToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route: status = %d — %s", resp.StatusCode, readAll(t, resp))
	}
}

// TestVideoRegisterTaskIdempotent 上游复用同一 id 时覆盖落点，不产生重复行。
func TestVideoRegisterTaskIdempotent(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)

	n := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"video_fixed","status":"queued"}`)
	}))
	defer up.Close()
	seedVideoModel(t, st, up.URL+"/v1")

	for i := 0; i < 2; i++ {
		if resp := typedPost(t, h, "/v1/videos", map[string]any{"model": "vid", "prompt": "x"}, vkToken); resp.StatusCode != 200 {
			t.Fatalf("submit %d: %d — %s", i, resp.StatusCode, readAll(t, resp))
		}
	}
	var tasks []store.VideoTask
	st.DB.Where("video_id = ?", "video_fixed").Find(&tasks)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1 (upstream reused id %d times)", len(tasks), n)
	}
	if tasks[0].CreatedAt == 0 {
		t.Errorf("created_at wiped on re-submit; retention purge would drop the mapping immediately")
	}
}
