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
// Username/Password = 管理台登录账号；
// 全空 = 完全开放（纯本地使用）。
// /v1 网关调用现已使用虚拟密钥鉴权（在 Web 管理界面创建和管理）。
type AdminAuth struct {
	Username string
	Password string
}

// Mode 返回管理面鉴权模式："password"（需登录）或 "open"（免登录）。
func (a AdminAuth) Mode() string {
	if a.Username != "" {
		return "password"
	}
	return "open"
}

// V1Protected 代理面是否需要凭据：现已全部使用虚拟密钥鉴权，此方法已废弃。
func (a AdminAuth) V1Protected() bool {
	return true // 始终需要虚拟密钥
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
func (s *Server) authorizeAdmin(r *http.Request) bool {
	if t := bearerToken(r); t != "" && s.sessions.check(t) {
		return true
	}
	if cred, ok := basicPayload(r); ok && s.auth.verifyCred(cred) {
		return true
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
