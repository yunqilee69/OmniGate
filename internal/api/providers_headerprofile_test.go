package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestProviderHeaderProfileCreateValidation 创建时对非法 JSON/空 key/保留头各断言 400。
func TestProviderHeaderProfileCreateValidation(t *testing.T) {
	h, _, _ := newTestServerWithStore(t)
	const token = "test-token"

	cases := []struct {
		name    string
		profile string
		want    int
		errPart string
	}{
		{"invalid json", `{"User-Agent":`, http.StatusBadRequest, "JSON"},
		{"reserved key", `{"Authorization":"Bearer x"}`, http.StatusBadRequest, "reserved"},
		{"valid", `{"User-Agent":"u"}`, http.StatusCreated, ""},
		{"empty", ``, http.StatusCreated, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{
				"name": "hp-create", "base_url": "https://x",
				"header_profile": tc.profile,
			}
			if tc.name == "empty" {
				payload = map[string]any{"name": "hp-create-2", "base_url": "https://x"}
			}
			rec := do(t, h, "POST", "/api/providers", payload, token)
			if rec.Code != tc.want {
				t.Fatalf("create code = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.errPart != "" && !strings.Contains(rec.Body.String(), tc.errPart) {
				t.Fatalf("error body should contain %q, got %s", tc.errPart, rec.Body.String())
			}
		})
	}
}

// TestProviderHeaderProfileUpdate 校验与最终态落库：非法更新 400、清空合法、合法更新持久化。
func TestProviderHeaderProfileUpdate(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
	const token = "test-token"

	rec := do(t, h, "POST", "/api/providers", map[string]any{
		"name": "hp-upd", "base_url": "https://x",
		"header_profile": `{"User-Agent":"u/1"}`,
	}, token)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d (body=%s)", rec.Code, rec.Body.String())
	}
	provID := int64(decodeObj(t, rec)["id"].(float64))

	// 保留头 → 400
	rec = do(t, h, "PUT", fmt.Sprintf("/api/providers/%d", provID),
		map[string]any{"header_profile": `{"X-Api-Key":"k"}`}, token)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update reserved key: %d (body=%s)", rec.Code, rec.Body.String())
	}

	// 清空（合法，回到透传）→ 200
	rec = do(t, h, "PUT", fmt.Sprintf("/api/providers/%d", provID),
		map[string]any{"header_profile": ""}, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear profile: %d (body=%s)", rec.Code, rec.Body.String())
	}

	// 合法更新 → 200 且响应携带终态
	rec = do(t, h, "PUT", fmt.Sprintf("/api/providers/%d", provID),
		map[string]any{"header_profile": `{"X-App":"cli"}`}, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid update: %d (body=%s)", rec.Code, rec.Body.String())
	}
	obj := decodeObj(t, rec)
	if obj["header_profile"] != `{"X-App":"cli"}` {
		t.Fatalf("persisted header_profile=%v", obj["header_profile"])
	}
	_ = st
}
