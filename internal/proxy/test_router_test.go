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
	return api.New(st, rtm, api.AdminAuth{}, ph, ph).Router()
}

func newStackWithRTM(t *testing.T) (*store.Store, *config.RuntimeManager) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
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

func newStackWithRTMAndVK(t *testing.T) (*store.Store, *config.RuntimeManager, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rt, err := config.NewRuntimeManager(st)
	if err != nil {
		t.Fatalf("init runtime: %v", err)
	}
	
	// 创建测试虚拟 key
	vk := &store.VirtualKey{
		Name:          "test-key",
		Status:        "active",
		RPMLimit:      0,
		TPMLimit:      0,
		BudgetUSD:     0,
		BudgetReset:   "never",
		AllowedModels: "[]",
	}
	if err := st.CreateVirtualKey(vk); err != nil {
		t.Fatal(err)
	}
	
	return st, rt, vk.KeyValue
}
