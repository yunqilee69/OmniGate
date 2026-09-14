package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cloudomni/omnigate/internal/store"
)

// TestLogsVirtualKeyNameAndFilter 回归：日志列表必须回显虚拟密钥名称，并支持 vk_id 过滤。
func TestLogsVirtualKeyNameAndFilter(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
	now := time.Now().Unix()

	alice := &store.VirtualKey{Name: "alice-bot", Status: "active"}
	bob := &store.VirtualKey{Name: "bob-bot", Status: "active"}
	for _, vk := range []*store.VirtualKey{alice, bob} {
		if err := st.CreateVirtualKey(vk); err != nil {
			t.Fatalf("create vk %s: %v", vk.Name, err)
		}
	}

	rows := []store.RequestLog{
		{RequestID: "vk-a-1", Route: "glm", Model: "glm-5", Provider: "Zhipu", Status: "success", VKID: alice.ID, CreatedAt: now},
		{RequestID: "vk-a-2", Route: "glm", Model: "glm-5", Provider: "Zhipu", Status: "error", VKID: alice.ID, CreatedAt: now},
		{RequestID: "vk-b-1", Route: "glm", Model: "glm-5", Provider: "Zhipu", Status: "success", VKID: bob.ID, CreatedAt: now},
		{RequestID: "vk-none", Route: "glm", Model: "glm-5", Provider: "Zhipu", Status: "success", CreatedAt: now},
	}
	for i := range rows {
		if err := st.DB.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}

	all := decodeObj(t, do(t, h, "GET", "/api/logs", nil, "test-token"))
	if all["total"].(float64) != 4 {
		t.Fatalf("unfiltered total = %v, want 4", all["total"])
	}
	names := map[string]string{}
	for _, it := range all["items"].([]any) {
		row := it.(map[string]any)
		names[row["request_id"].(string)] = fmt.Sprint(row["vk_name"])
	}
	if names["vk-a-1"] != "alice-bot" || names["vk-a-2"] != "alice-bot" || names["vk-b-1"] != "bob-bot" {
		t.Fatalf("vk_name echo wrong: %v", names)
	}
	if names["vk-none"] != "" {
		t.Fatalf("missing vk should echo empty name, got %q", names["vk-none"])
	}

	filtered := decodeObj(t, do(t, h, "GET", fmt.Sprintf("/api/logs?vk_id=%d", alice.ID), nil, "test-token"))
	if filtered["total"].(float64) != 2 {
		t.Fatalf("vk_id filter total = %v, want 2", filtered["total"])
	}
	for _, it := range filtered["items"].([]any) {
		row := it.(map[string]any)
		if row["vk_name"] != "alice-bot" {
			t.Fatalf("filtered row vk_name = %v, want alice-bot", row["vk_name"])
		}
		if int64(row["vk_id"].(float64)) != alice.ID {
			t.Fatalf("filtered row vk_id = %v, want %d", row["vk_id"], alice.ID)
		}
	}

	bad := do(t, h, "GET", "/api/logs?vk_id=abc", nil, "test-token")
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid vk_id should be 400, got %d — %s", bad.Code, bad.Body.String())
	}

	detail := do(t, h, "GET", "/api/logs/vk-a-1", nil, "test-token")
	if detail.Code != http.StatusOK {
		t.Fatalf("log detail: %d — %s", detail.Code, detail.Body.String())
	}
	var payload struct {
		VKName string `json:"vk_name"`
		Log    struct {
			VKID int64 `json:"vk_id"`
		} `json:"log"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if payload.VKName != "alice-bot" || payload.Log.VKID != alice.ID {
		t.Fatalf("detail vk echo = %q / %d, want alice-bot / %d", payload.VKName, payload.Log.VKID, alice.ID)
	}

	if err := st.DB.Delete(&store.VirtualKey{}, alice.ID).Error; err != nil {
		t.Fatalf("delete vk: %v", err)
	}
	after := decodeObj(t, do(t, h, "GET", fmt.Sprintf("/api/logs?vk_id=%d", alice.ID), nil, "test-token"))
	if after["total"].(float64) != 2 {
		t.Fatalf("deleted vk filter total = %v, want 2", after["total"])
	}
	for _, it := range after["items"].([]any) {
		row := it.(map[string]any)
		if row["vk_name"] != "" {
			t.Fatalf("deleted vk should echo empty name, got %v", row["vk_name"])
		}
		if int64(row["vk_id"].(float64)) != alice.ID {
			t.Fatalf("deleted vk must keep vk_id, got %v", row["vk_id"])
		}
	}
}
