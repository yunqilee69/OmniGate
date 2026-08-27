package proxy_test

import (
	"net/http"

	"github.com/cloudomni/omnigate/internal/api"
	"github.com/cloudomni/omnigate/internal/config"
	"github.com/cloudomni/omnigate/internal/proxy"
	"github.com/cloudomni/omnigate/internal/store"
)

func apiTestRouter(st *store.Store, ph *proxy.Handler, rtm *config.RuntimeManager) http.Handler {
	return api.New(st, rtm, "", ph).Router()
}
