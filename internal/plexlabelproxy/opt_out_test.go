package plexlabelproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// proxyTokens sends each request through a proxy built from cfg and returns
// the X-Plex-Token each one reached Plex with.
func proxyTokens(t *testing.T, cfg Config, reqs []*http.Request) []string {
	t.Helper()
	var tokens []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.URL.Query().Get("X-Plex-Token"))
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	cfg.Upstream = up.URL
	cfg.Token = "label-token"
	cfg.OwnerToken = "owner-token"
	cfg.ElevateLiveTV = true
	proxy, err := New(cfg)
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	for _, req := range reqs {
		proxy.ServeHTTP(httptest.NewRecorder(), req)
	}
	if len(tokens) != len(reqs) {
		t.Fatalf("Plex saw %d requests, want %d", len(tokens), len(reqs))
	}
	return tokens
}

func subscriptionRequests() []*http.Request {
	return []*http.Request{
		httptest.NewRequest(http.MethodGet,
			"/media/subscriptions/template?X-Plex-Token=user-token&guid=tv.plex.xmltv%3A%2F%2Fmovie%2Fx", nil),
		httptest.NewRequest(http.MethodPost,
			"/media/subscriptions?X-Plex-Token=user-token&hints%5Bkey%5D=%2Ftv.plex.providers.epg.xmltv%3A123%2Fgrid", nil),
		httptest.NewRequest(http.MethodGet, "/livetv/dvrs?X-Plex-Token=user-token", nil),
	}
}

func TestProxy_SubscriptionsElevatedByDefault(t *testing.T) {
	got := proxyTokens(t, Config{}, subscriptionRequests())
	for i, want := range []string{"owner-token", "owner-token", "owner-token"} {
		if got[i] != want {
			t.Errorf("request %d reached Plex with %q, want %q", i, got[i], want)
		}
	}
}

func TestProxy_NoElevateSubscriptionsKeepsRecordingsUserScoped(t *testing.T) {
	got := proxyTokens(t, Config{NoElevateSubscriptions: true}, subscriptionRequests())
	// Recording requests keep the user's token; other Live TV requests are still elevated
	for i, want := range []string{"user-token", "user-token", "owner-token"} {
		if got[i] != want {
			t.Errorf("request %d reached Plex with %q, want %q", i, got[i], want)
		}
	}
}

func identityRequests() []*http.Request {
	var reqs []*http.Request
	for _, target := range []string{"/identity?X-Plex-Token=user-token", "/?X-Plex-Token=user-token"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Referer", "https://plex.example/livetv/sessions/abc")
		reqs = append(reqs, req)
	}
	return reqs
}

func TestProxy_RootIdentityElevatedByDefault(t *testing.T) {
	for i, got := range proxyTokens(t, Config{}, identityRequests()) {
		if got != "owner-token" {
			t.Errorf("request %d reached Plex with %q, want the owner token", i, got)
		}
	}
}

func TestProxy_NoElevateRootIdentityKeepsUserToken(t *testing.T) {
	for i, got := range proxyTokens(t, Config{NoElevateRootIdentity: true}, identityRequests()) {
		if got != "user-token" {
			t.Errorf("request %d reached Plex with %q, want the user's token", i, got)
		}
	}
}
