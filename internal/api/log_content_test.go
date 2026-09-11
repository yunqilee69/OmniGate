package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cloudomni/omnigate/internal/store"
)

// TestLogContentRoute：/api/logs/{request_id}/content 路由必须存在（历史缺失导致前端永远看不到捕获内容）。
// 有记录时返回客户端入站、上游出站、上游响应三组内容；无记录时返回 no_content 业务提示（区别于路由级 404）。
func TestLogContentRoute(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)

	cl := store.ContentLog{
		RequestID: "req-content-1", Route: "glm-pool",
		ClientRequestHeaders: "Content-Type: application/json\n", ClientRequestBody: `{"model":"glm-pool"}`,
		RequestHeaders: "Content-Type: application/json\n", RequestBody: `{"model":"m"}`,
		ResponseHeaders: "Content-Type: application/json\n", ResponseBody: `{"ok":true}`,
	}
	if err := st.DB.Create(&cl).Error; err != nil {
		t.Fatal(err)
	}

	rec := do(t, h, http.MethodGet, "/api/logs/req-content-1/content", nil, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("get content: %d — %s", rec.Code, rec.Body.String())
	}
	var got store.ContentLog
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode content: %v", err)
	}
	if got.ClientRequestHeaders == "" || got.RequestHeaders == "" || got.ResponseHeaders == "" {
		t.Fatalf("header fields missing in response: %+v", got)
	}
	if got.ClientRequestBody == "" || got.RequestBody == "" || got.ResponseBody == "" {
		t.Fatalf("body fields missing in response: %+v", got)
	}

	// 无记录：404 + no_content 提示（不是路由级 "not found"）
	rec = do(t, h, http.MethodGet, "/api/logs/req-none/content", nil, "test-token")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing content should be 404, got %d", rec.Code)
	}
	obj := decodeObj(t, rec)
	errObj, _ := obj["error"].(map[string]any)
	if errObj["code"] != "no_content" {
		t.Fatalf("expect no_content error code, got: %s", rec.Body.String())
	}
}
