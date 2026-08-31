// Package api 实现管理面 REST API 与代理面入口（M1：实体 CRUD + 配置 + 健康）。
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/cloudomni/omnigate/internal/config"
	"github.com/cloudomni/omnigate/internal/store"
	"github.com/cloudomni/omnigate/internal/webui"
)

// Server 管理面 HTTP 服务。
type Server struct {
	store    *store.Store
	rt       *config.RuntimeManager
	auth     AdminAuth
	sessions *sessionStore
	chat     ChatPlane
	typed    TypedPlane
}

// ChatPlane chat 端点族（/v1/chat/completions、/v1/messages、/v1/responses）的代理处理器集合。
type ChatPlane interface {
	http.Handler
	Messages(w http.ResponseWriter, r *http.Request)
	Responses(w http.ResponseWriter, r *http.Request)
}

// TypedPlane 非 chat 端点族（/v1/embeddings、/v1/rerank）的代理处理器集合。
type TypedPlane interface {
	Embeddings(w http.ResponseWriter, r *http.Request)
	Rerank(w http.ResponseWriter, r *http.Request)
}

// New 构造管理面服务。auth 为启动层静态鉴权配置（详见 AdminAuth）；
// chat 为 chat 端点族处理器，typed 为 embeddings/rerank 处理器（均可为 nil，测试场景）。
func New(st *store.Store, rt *config.RuntimeManager, auth AdminAuth, chat ChatPlane, typed TypedPlane) *Server {
	return &Server{store: st, rt: rt, auth: auth, sessions: newSessionStore(), chat: chat, typed: typed}
}

// Router 组装全部 HTTP 路由。
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(corsMiddleware)

	// 公开端点：登录页引导与凭据校验，不经过 authMW
	r.Get("/api/auth-info", s.handleAuthInfo)
	r.Post("/api/login", s.handleLogin)

	r.Route("/api", func(ar chi.Router) {
		ar.Use(s.authMW)
		ar.Get("/health", s.getHealth)
		ar.Get("/settings", s.getSettings)
		ar.Put("/settings", s.putSettings)

		ar.Get("/stats/overview", s.getStatsOverview)
		ar.Get("/stats/breakdown", s.getStatsBreakdown)
		ar.Get("/stats/timeseries", s.getStatsTimeseries)
		ar.Get("/logs", s.getLogs)
		ar.Get("/logs/{request_id}/content", s.getLogContent)
		ar.Get("/logs/{request_id}/attempts", s.getLogAttempts)
		ar.Get("/logs/{request_id}", s.getLogByID)

		ar.Post("/maintenance/cleanup", s.postMaintenanceCleanup)
		ar.Post("/maintenance/clear-stats", s.postMaintenanceClearStats)
		ar.Post("/logout", s.handleLogout)

		ar.Route("/providers", func(er chi.Router) {
			er.Get("/", s.listProviders)
			er.Post("/", s.createProvider)
			er.Route("/{id}", func(ir chi.Router) {
				ir.Put("/", s.updateProvider)
				ir.Delete("/", s.deleteProvider)
				ir.Post("/test", s.testProvider)
			})
		})
		ar.Route("/keys", func(er chi.Router) {
			er.Get("/", s.listKeys)
			er.Post("/", s.createKeys)
			er.Route("/{id}", func(ir chi.Router) {
				ir.Put("/", s.updateKey)
				ir.Delete("/", s.deleteKey)
			})
		})
		ar.Route("/models", func(er chi.Router) {
			er.Get("/", s.listModels)
			er.Post("/", s.createModel)
			er.Route("/{id}", func(ir chi.Router) {
				ir.Put("/", s.updateModel)
				ir.Delete("/", s.deleteModel)
				ir.Post("/enable", s.enableModel)
				ir.Post("/disable", s.disableModel)
				ir.Post("/test", s.testModel)
				ir.Post("/test-keys", s.testModelKeys)
			})
		})
		ar.Route("/routes", func(er chi.Router) {
			er.Get("/", s.listRoutes)
			er.Post("/", s.createRoute)
			er.Route("/{id}", func(ir chi.Router) {
				ir.Put("/", s.updateRoute)
				ir.Delete("/", s.deleteRoute)
			})
		})
	})

	r.With(s.v1AuthMW).Get("/v1/models", s.v1Models)
	if s.chat != nil {
		r.With(s.v1AuthMW).Post("/v1/chat/completions", s.chat.ServeHTTP)
		r.With(s.v1AuthMW).Post("/v1/messages", s.chat.Messages)
		r.With(s.v1AuthMW).Post("/v1/responses", s.chat.Responses)
	}
	if s.typed != nil {
		r.With(s.v1AuthMW).Post("/v1/embeddings", s.typed.Embeddings)
		r.With(s.v1AuthMW).Post("/v1/rerank", s.typed.Rerank)
	}
	r.NotFound(webui.Handler().ServeHTTP)
	return r
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write response failed", "err", err)
	}
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id, err == nil && id > 0
}

func decodeBody(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return errors.New("empty request body")
	}
	return json.Unmarshal(body, v)
}

func isUniqueErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// maskKey 密钥脱敏展示：保留前 5 后 4。
func maskKey(kv string) string {
	if len(kv) <= 8 {
		return "****"
	}
	return kv[:5] + "****" + kv[len(kv)-4:]
}
