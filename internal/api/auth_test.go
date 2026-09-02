package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cloudomni/omnigate/internal/config"
	"github.com/cloudomni/omnigate/internal/store"
)

func doReq(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func newAuthTestServer(t *testing.T, auth AdminAuth) http.Handler {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rt, err := config.NewRuntimeManager(st)
	if err != nil {
		t.Fatalf("init runtime config: %v", err)
	}
	return New(st, rt, auth, nil, nil).Router()
}

func decodeMap(t *testing.T, res string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(res), &m); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	return m
}

func TestAuthInfoModes(t *testing.T) {
	for _, tc := range []struct {
		auth AdminAuth
		want string
	}{
		{AdminAuth{}, "open"},
		{AdminAuth{ApiKey: "sk-gw"}, "open"},
		{AdminAuth{Username: "admin", Password: "pw"}, "password"},
	} {
		h := newAuthTestServer(t, tc.auth)
		res := do(t, h, "GET", "/api/auth-info", nil, "")
		if res.Code != http.StatusOK {
			t.Fatalf("auth-info status = %d", res.Code)
		}
		if got := decodeMap(t, res.Body.String())["mode"]; got != tc.want {
			t.Fatalf("mode = %v, want %v", got, tc.want)
		}
	}
}

func TestLoginPasswordMode(t *testing.T) {
	h := newAuthTestServer(t, AdminAuth{Username: "admin", Password: "s3cret"})

	res := do(t, h, "POST", "/api/login", map[string]string{"username": "admin", "password": "wrong"}, "")
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want 401", res.Code)
	}

	res = do(t, h, "POST", "/api/login", map[string]string{"username": "admin", "password": "s3cret"}, "")
	if res.Code != http.StatusOK {
		t.Fatalf("login status = %d, body %s", res.Code, res.Body.String())
	}
	tok, _ := decodeMap(t, res.Body.String())["token"].(string)
	if tok == "" {
		t.Fatal("login did not issue session token")
	}

	res = do(t, h, "GET", "/api/health", nil, "")
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("no credential status = %d, want 401", res.Code)
	}
	req, _ := http.NewRequest("GET", "/api/health", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := doReq(t, h, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("bearer session status = %d, want 200", rr.Code)
	}

	// 登出后会话立即失效
	req, _ = http.NewRequest("POST", "/api/logout", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if rr := doReq(t, h, req); rr.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d", rr.Code)
	}
	req, _ = http.NewRequest("GET", "/api/health", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if rr := doReq(t, h, req); rr.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session status = %d, want 401", rr.Code)
	}
}

func TestApiKeyNotValidOnAdminPlane(t *testing.T) {
	// api_key 是 /v1 专用凭据,不得打开管理面
	h := newAuthTestServer(t, AdminAuth{Username: "admin", Password: "s3cret", ApiKey: "sk-gw"})
	req, _ := http.NewRequest("GET", "/api/health", nil)
	req.Header.Set("Authorization", "Bearer sk-gw")
	if rr := doReq(t, h, req); rr.Code != http.StatusUnauthorized {
		t.Fatalf("api_key on admin plane status = %d, want 401", rr.Code)
	}
}

func TestV1ApiKeyOnlyMode(t *testing.T) {
	// 验证 /v1 端点现在需要虚拟 key 而非旧 API key
	h := newAuthTestServer(t, AdminAuth{ApiKey: "sk-gw"})

	if res := do(t, h, "GET", "/api/health", nil, ""); res.Code != http.StatusOK {
		t.Fatalf("admin open status = %d, want 200", res.Code)
	}
	
	// 旧 API key 不再工作 - 现在需要虚拟 key
	req, _ := http.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-gw")
	if rr := doReq(t, h, req); rr.Code != http.StatusUnauthorized {
		t.Fatalf("old api_key should be rejected, got status = %d", rr.Code)
	}
}
func TestV1CredentialForms(t *testing.T) {
	// 验证 /v1 端点现在只接受虚拟 key，旧凭据形式都被拒绝
	enc := base64.StdEncoding.EncodeToString([]byte("admin:s3cret"))
	h := newAuthTestServer(t, AdminAuth{Username: "admin", Password: "s3cret", ApiKey: "sk-gw"})

	res := do(t, h, "GET", "/v1/models", nil, "")
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("no credential status = %d, want 401", res.Code)
	}
	if www := res.Header().Get("WWW-Authenticate"); www == "" {
		t.Fatal("401 missing WWW-Authenticate header (RFC 7617)")
	}

	// 所有旧的鉴权形式现在都应该被拒绝
	for _, tc := range []struct {
		name   string
		header string
	}{
		{"basic", "Basic " + enc},
		{"bearer-encoded", "Bearer " + enc},
		{"bearer-raw", "Bearer admin:s3cret"},
		{"bearer-api-key", "Bearer sk-gw"},
	} {
		req, _ := http.NewRequest("GET", "/v1/models", nil)
		req.Header.Set("Authorization", tc.header)
		if rr := doReq(t, h, req); rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s should be rejected, got status = %d", tc.name, rr.Code)
		}
	}
}

func TestV1OpenWhenNothingConfigured(t *testing.T) {
	// 验证即使没有配置管理员凭据，/v1 仍需要虚拟 key
	h := newAuthTestServer(t, AdminAuth{})
	if res := do(t, h, "GET", "/v1/models", nil, ""); res.Code != http.StatusUnauthorized {
		t.Fatalf("v1 should require VK even when no admin auth, got status = %d", res.Code)
	}
}

func TestAdminPlaneBasicAuth(t *testing.T) {
	h := newAuthTestServer(t, AdminAuth{Username: "admin", Password: "s3cret"})
	req, _ := http.NewRequest("GET", "/api/health", nil)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("admin:s3cret")))
	if rr := doReq(t, h, req); rr.Code != http.StatusOK {
		t.Fatalf("admin plane basic status = %d, body %s", rr.Code, rr.Body.String())
	}
}
