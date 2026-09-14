package api

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/cloudomni/omnigate/internal/store"
)

// TestRouteFallbackModelID 校验路由级兜底模型：协议/类型不匹配或不存在 → 400；
func TestRouteFallbackModelID(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
	token := "test-token"

	// 自建 provider + chat/completions 目标模型与兜底候选。
	p := store.Provider{Name: "fb-provider", BaseURL: "http://127.0.0.1:1"}
	if err := st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	target := store.Model{ProviderID: p.ID, Name: "fb-target", Type: "chat", Protocol: "completions"}
	if err := st.DB.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	rec := do(t, h, "POST", "/api/routes", map[string]any{
		"name": "fb-route",
		"targets": []map[string]any{
			{"model_id": target.ID, "weight": 1},
		},
	}, token)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create route: %d — %s", rec.Code, rec.Body.String())
	}
	routeID := idOf(t, decodeObj(t, rec))

	// 不存在的模型 → 400
	rec = do(t, h, "PUT", fmt.Sprintf("/api/routes/%d", routeID), map[string]any{
		"fallback_model_id": 999999,
	}, token)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing fallback model: expect 400, got %d", rec.Code)
	}

	// 负数 → 400
	rec = do(t, h, "PUT", fmt.Sprintf("/api/routes/%d", routeID), map[string]any{
		"fallback_model_id": -1,
	}, token)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative fallback id: expect 400, got %d", rec.Code)
	}

	// 类型不匹配（embedding 模型兜 chat 路由）→ 400
	embModel := store.Model{ProviderID: p.ID, Name: "emb-x", Type: "embedding", Protocol: "completions"}
	if err := st.DB.Create(&embModel).Error; err != nil {
		t.Fatal(err)
	}
	rec = do(t, h, "PUT", fmt.Sprintf("/api/routes/%d", routeID), map[string]any{
		"fallback_model_id": embModel.ID,
	}, token)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("type-mismatched fallback: expect 400, got %d", rec.Code)
	}

	// 协议不匹配（messages 协议模型兜 completions 路由）→ 400
	msgModel := store.Model{ProviderID: embModel.ProviderID, Name: "msg-x", Type: "chat", Protocol: "messages"}
	if err := st.DB.Create(&msgModel).Error; err != nil {
		t.Fatal(err)
	}
	rec = do(t, h, "PUT", fmt.Sprintf("/api/routes/%d", routeID), map[string]any{
		"fallback_model_id": msgModel.ID,
	}, token)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("protocol-mismatched fallback: expect 400, got %d", rec.Code)
	}

	// 合法 chat 模型 → 200 且持久化
	chatModel := store.Model{ProviderID: embModel.ProviderID, Name: "chat-x", Type: "chat", Protocol: "completions"}
	if err := st.DB.Create(&chatModel).Error; err != nil {
		t.Fatal(err)
	}
	rec = do(t, h, "PUT", fmt.Sprintf("/api/routes/%d", routeID), map[string]any{
		"fallback_model_id": chatModel.ID,
	}, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid fallback: expect 200, got %d — %s", rec.Code, rec.Body.String())
	}
	var rt store.Route
	if err := st.DB.First(&rt, routeID).Error; err != nil {
		t.Fatal(err)
	}
	if rt.FallbackModelID != chatModel.ID {
		t.Fatalf("fallback_model_id = %d, want %d", rt.FallbackModelID, chatModel.ID)
	}

	// 显式 0 → 清除
	rec = do(t, h, "PUT", fmt.Sprintf("/api/routes/%d", routeID), map[string]any{
		"fallback_model_id": 0,
	}, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear fallback: expect 200, got %d", rec.Code)
	}
	if err := st.DB.First(&rt, routeID).Error; err != nil {
		t.Fatal(err)
	}
	if rt.FallbackModelID != 0 {
		t.Fatalf("fallback_model_id after clear = %d, want 0", rt.FallbackModelID)
	}
}
