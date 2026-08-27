package proxy_test

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/cloudomni/omnigate/internal/api"
	"github.com/cloudomni/omnigate/internal/config"
	"github.com/cloudomni/omnigate/internal/proxy"
	"github.com/cloudomni/omnigate/internal/store"
)

func apiTestRouter(st *store.Store, ph *proxy.Handler, rtm *config.RuntimeManager) http.Handler {
	return api.New(st, rtm, "", ph).Router()
}

func newStackWithRTM(t *testing.T) (*store.Store, *config.RuntimeManager) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	rt, err := config.NewRuntimeManager(st)
	if err != nil {
		t.Fatalf("init runtime: %v", err)
	}
	return st, rt
}

func hWithRTM(st *store.Store, rtm *config.RuntimeManager) http.Handler {
	ph := proxy.New(st, rtm)
	return apiTestRouter(st, ph, rtm)
}
