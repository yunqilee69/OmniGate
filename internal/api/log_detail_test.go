package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cloudomni/omnigate/internal/store"
)

// TestLogDetailAttemptKeys 回归：尝试链路每跳可能落到不同密钥，明细必须按各跳的 key_id 取名称，
// 而不是复用请求终态密钥，也不能退化成裸 ID。
func TestLogDetailAttemptKeys(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)

	prov := store.Provider{Name: "zhipu", BaseURL: "https://api.example.com", Protocol: "completions"}
	if err := st.DB.Create(&prov).Error; err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	first := store.ApiKey{ProviderID: prov.ID, KeyValue: "sk-first-111122223333", Name: "主账号"}
	second := store.ApiKey{ProviderID: prov.ID, KeyValue: "sk-second-444455556666", Name: "备用账号"}
	for _, k := range []*store.ApiKey{&first, &second} {
		if err := st.DB.Create(k).Error; err != nil {
			t.Fatalf("seed key: %v", err)
		}
	}

	log := store.RequestLog{
		RequestID: "req-attempt-keys", Route: "glm", Model: "m", Provider: "zhipu",
		KeyID: second.ID, Status: "success", CreatedAt: 1700000000,
	}
	attempts := []store.RequestAttempt{
		{RequestID: log.RequestID, Attempt: 0, Route: "glm", Model: "m1", Provider: "zhipu",
			KeyID: first.ID, Status: "error", ErrorCode: "500"},
		{RequestID: log.RequestID, Attempt: 1, Route: "glm", Model: "m2", Provider: "zhipu",
			KeyID: second.ID, Status: "success"},
	}
	if err := st.SettleRequest(&log, attempts); err != nil {
		t.Fatalf("settle: %v", err)
	}

	assertAttempts := func(path string) {
		t.Helper()
		rec := do(t, h, http.MethodGet, path, nil, "test-token")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d — %s", path, rec.Code, rec.Body.String())
		}
		var payload struct {
			Attempts []struct {
				Attempt        int    `json:"attempt"`
				KeyID          int64  `json:"key_id"`
				KeyName        string `json:"key_name"`
				KeyValueMasked string `json:"key_value_masked"`
			} `json:"attempts"`
		}
		body := rec.Body.Bytes()
		if path == "/api/logs/"+log.RequestID+"/attempts" {
			var rows []struct {
				Attempt        int    `json:"attempt"`
				KeyID          int64  `json:"key_id"`
				KeyName        string `json:"key_name"`
				KeyValueMasked string `json:"key_value_masked"`
			}
			if err := json.Unmarshal(body, &rows); err != nil {
				t.Fatalf("decode attempts: %v", err)
			}
			payload.Attempts = rows
		} else if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode detail: %v", err)
		}

		if len(payload.Attempts) != 2 {
			t.Fatalf("GET %s: want 2 attempts, got %d", path, len(payload.Attempts))
		}
		if payload.Attempts[0].KeyName != "主账号" || payload.Attempts[1].KeyName != "备用账号" {
			t.Fatalf("GET %s: per-attempt key names wrong: %+v", path, payload.Attempts)
		}
		if payload.Attempts[0].KeyValueMasked == "" || payload.Attempts[0].KeyValueMasked == first.KeyValue {
			t.Fatalf("GET %s: attempt key must be masked, got %q", path, payload.Attempts[0].KeyValueMasked)
		}
	}

	assertAttempts("/api/logs/" + log.RequestID)
	assertAttempts("/api/logs/" + log.RequestID + "/attempts")

	// 密钥被删除后，行仍在，名称为空（前端回退到 key#id），接口不得报错。
	if err := st.DB.Delete(&store.ApiKey{}, first.ID).Error; err != nil {
		t.Fatalf("delete key: %v", err)
	}
	rec := do(t, h, http.MethodGet, "/api/logs/"+log.RequestID, nil, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("after key delete: %d — %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Attempts []struct {
			KeyID          int64  `json:"key_id"`
			KeyName        string `json:"key_name"`
			KeyValueMasked string `json:"key_value_masked"`
		} `json:"attempts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if len(payload.Attempts) != 2 || payload.Attempts[0].KeyName != "" || payload.Attempts[0].KeyValueMasked != "" {
		t.Fatalf("deleted key should yield empty name/mask: %+v", payload.Attempts)
	}
	if payload.Attempts[0].KeyID != first.ID {
		t.Fatalf("deleted key attempt must keep key_id, got %d", payload.Attempts[0].KeyID)
	}
}
