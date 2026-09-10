package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudomni/omnigate/internal/store"
)

// TestProviderHeaderProfilesCreateValidation 创建时对非法 JSON/保留头/生效组不匹配各断言 400。
func TestProviderHeaderProfilesCreateValidation(t *testing.T) {
	h, _, _ := newTestServerWithStore(t)
	const token = "test-token"

	cases := []struct {
		name     string
		profiles string
		active   string
		want     int
		wantMsg  string
	}{
		{"invalid json", "not-json", "", http.StatusBadRequest, "JSON 非法"},
		{"reserved key", `[{"name":"g","headers":{"Authorization":"Bearer x"}}]`, "", http.StatusBadRequest, "reserved"},
		{"active not found", `[{"name":"g","headers":{"User-Agent":"u"}}]`, "other", http.StatusBadRequest, "active_profile"},
		{"valid", `[{"name":"g","headers":{"User-Agent":"u"}}]`, "g", http.StatusCreated, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, "POST", "/api/providers", map[string]any{
				"name": "hp-create", "base_url": "https://x",
				"header_profiles": tc.profiles, "active_profile": tc.active,
			}, token)
			if rec.Code != tc.want {
				t.Fatalf("create code = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.wantMsg != "" && !strings.Contains(rec.Body.String(), tc.wantMsg) {
				t.Fatalf("body %q does not contain %q", rec.Body.String(), tc.wantMsg)
			}
		})
	}
}

// TestProviderHeaderProfilesUpdate 合并校验与最终态落库：单改生效组、清空组、合法更新各路径。
func TestProviderHeaderProfilesUpdate(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
	const token = "test-token"

	rec := do(t, h, "POST", "/api/providers", map[string]any{
		"name": "hp-upd", "base_url": "https://x",
		"header_profiles": `[{"name":"g1","headers":{"User-Agent":"u/1"}}]`, "active_profile": "g1",
	}, token)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d (body=%s)", rec.Code, rec.Body.String())
	}
	provID := idOf(t, decodeObj(t, rec))

	// 单改 active_profile 为不存在的组 → 400
	rec = do(t, h, "PUT", fmt.Sprintf("/api/providers/%d", provID), map[string]any{"active_profile": "zzz"}, token)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update active to missing group: %d (body=%s)", rec.Code, rec.Body.String())
	}

	// 清除生效组（合法）→ 200
	rec = do(t, h, "PUT", fmt.Sprintf("/api/providers/%d", provID), map[string]any{"active_profile": ""}, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear active: %d (body=%s)", rec.Code, rec.Body.String())
	}

	// 清空组但 active_profile 仍指向旧组 → 400（合并校验）
	rec = do(t, h, "PUT", fmt.Sprintf("/api/providers/%d", provID), map[string]any{
		"active_profile": "g1", "header_profiles": "",
	}, token)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("clear groups with dangling active: %d (body=%s)", rec.Code, rec.Body.String())
	}

	// 合法更新：新组 + 生效组
	rec = do(t, h, "PUT", fmt.Sprintf("/api/providers/%d", provID), map[string]any{
		"header_profiles": `[{"name":"g2","headers":{"X-App":"cli"}}]`, "active_profile": "g2",
	}, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid update: %d (body=%s)", rec.Code, rec.Body.String())
	}

	var p store.Provider
	if err := st.DB.First(&p, provID).Error; err != nil {
		t.Fatal(err)
	}
	if p.HeaderProfiles != `[{"name":"g2","headers":{"X-App":"cli"}}]` || p.ActiveProfile != "g2" {
		t.Fatalf("persisted header_profiles=%q active_profile=%q", p.HeaderProfiles, p.ActiveProfile)
	}
}
