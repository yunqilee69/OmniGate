package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

const sessionTTL = 7 * 24 * time.Hour

// AdminAuth 启动层鉴权配置（config.yaml 静态注入，重启生效）：
// Username/Password = 管理台登录账号，同时可作 /v1 调用凭据（Basic 规则）；
// ApiKey = /v1 专用网关调用密钥，不用于管理面；
// 全空 = 完全开放（纯本地使用）。
type AdminAuth struct {
	Username string
	Password string
	ApiKey   string
}

// Mode 返回管理面鉴权模式："password"（需登录）或 "open"（免登录）。
func (a AdminAuth) Mode() string {
	if a.Username != "" {
		return "password"
	}
	return "open"
}

// V1Protected 代理面是否需要凭据：账号密码或 api_key 任一设置即启用。
func (a AdminAuth) V1Protected() bool {
	return a.Username != "" || a.ApiKey != ""
}

// credential 拼装待比对凭据："用户名:密码"（与 Basic 解码后的形态一致）。
func (a AdminAuth) credential() string {
	return a.Username + ":" + a.Password
}

// verifyCred 常量时间比对凭据，避免逐字节短路造成的时序侧信道。
// 长度差异仍会泄露（ConstantTimeCompare 语义），凭据长度不属于敏感信息。
func (a AdminAuth) verifyCred(cred string) bool {
	return a.Username != "" && subtle.ConstantTimeCompare([]byte(cred), []byte(a.credential())) == 1
}

func (a AdminAuth) verifyToken(tok string) bool {
	return a.ApiKey != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(a.ApiKey)) == 1
}

// sessionStore 进程内会话表：登录签发随机令牌，重启即全部失效。
// 不落盘是有意为之——凭据本身是启动层静态配置，会话生命周期与进程对齐。
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string]time.Time)}
}

func (ss *sessionStore) issue() (string, int64) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	tok := hex.EncodeToString(b)
	exp := time.Now().Add(sessionTTL)
	ss.mu.Lock()
	ss.sessions[tok] = exp
	ss.mu.Unlock()
	return tok, exp.Unix()
}

func (ss *sessionStore) check(tok string) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	exp, ok := ss.sessions[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(ss.sessions, tok)
		return false
	}
	ss.sessions[tok] = time.Now().Add(sessionTTL) // 滑动续期：活跃会话不掉线
	return true
}

func (ss *sessionStore) revoke(tok string) {
	ss.mu.Lock()
	delete(ss.sessions, tok)
	ss.mu.Unlock()
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// basicPayload 解码 Basic 头为 "用户名:密码" 原文；非法 base64 视为无凭据。
func basicPayload(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) > 6 && strings.EqualFold(h[:6], "Basic ") {
		if raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[6:])); err == nil {
			return string(raw), true
		}
	}
	return "", false
}

// authorizeAdmin 管理面放行规则（仅账号密码模式拦截）：
// Bearer 会话令牌 / Basic 账号密码，任一命中即可。
// api_key 是 /v1 专用凭据，刻意不在管理面放行，避免调用密钥泄露即拿到管理权。
func (s *Server) authorizeAdmin(r *http.Request) bool {
	if t := bearerToken(r); t != "" && s.sessions.check(t) {
		return true
	}
	if cred, ok := basicPayload(r); ok && s.auth.verifyCred(cred) {
		return true
	}
	return false
}

// authorizeV1 代理面放行规则（账号密码或 api_key 任一设置即拦截）：
// Basic base64(user:pass)（RFC 7617 标准）、Bearer base64(user:pass)（OpenAI SDK
// 直接把编码串填入 api_key）、Bearer "user:pass" 原文、Bearer api_key（配置的网关密钥）。
func (s *Server) authorizeV1(r *http.Request) bool {
	if cred, ok := basicPayload(r); ok && s.auth.verifyCred(cred) {
		return true
	}
	if t := bearerToken(r); t != "" {
		if s.auth.verifyCred(t) {
			return true
		}
		if raw, err := base64.StdEncoding.DecodeString(t); err == nil && s.auth.verifyCred(string(raw)) {
			return true
		}
		if s.auth.verifyToken(t) {
			return true
		}
	}
	return false
}

func (s *Server) authMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auth.Mode() == "open" || s.authorizeAdmin(r) {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid credentials")
	})
}

func (s *Server) v1AuthMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.V1Protected() || s.authorizeV1(r) {
			next.ServeHTTP(w, r)
			return
		}
		// RFC 7617：401 须携带 WWW-Authenticate，标准客户端据此弹出/填充 Basic 凭据
		w.Header().Set("WWW-Authenticate", `Basic realm="omnigate", charset="UTF-8"`)
		writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid credentials")
	})
}

// handleAuthInfo 公开端点：登录页据此渲染账号密码表单/令牌表单，或直接放行。
func (s *Server) handleAuthInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"mode": s.auth.Mode()})
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLogin 公开端点：账号密码校验后签发会话令牌。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if s.auth.Mode() != "password" {
		writeErr(w, http.StatusBadRequest, "auth_not_enabled", "服务端未启用登录")
		return
	}
	if !s.auth.verifyCred(req.Username + ":" + req.Password) {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "用户名或密码错误")
		return
	}
	tok, exp := s.sessions.issue()
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "expires_at": exp})
}

// handleLogout 撤销当前 Bearer 会话；静态令牌/Basic 为无状态凭据，登出仅清客户端。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if t := bearerToken(r); t != "" {
		s.sessions.revoke(t)
	}
	w.WriteHeader(http.StatusNoContent)
}
