package plexlabelproxy

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// pmsWithStatus answers /library/sections with whatever status is stored in
// status, counting the calls.
func pmsWithStatus(t *testing.T, status *atomic.Int32, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPlexTokenAuthorizer_PMSErrorIsNotCachedAsRefusal(t *testing.T) {
	var status, calls atomic.Int32
	status.Store(http.StatusServiceUnavailable)
	pms := pmsWithStatus(t, &status, &calls)
	a, err := NewPlexTokenAuthorizer(pms.URL, "owner-token", nil)
	if err != nil {
		t.Fatal(err)
	}

	d := a.AllowPlexTokenDetailed(context.Background(), "friend-token")
	if d.Allowed || !d.Unavailable {
		t.Fatalf("PMS 503 for an unknown token: got %+v, want refused and unavailable", d)
	}

	status.Store(http.StatusOK)
	d = a.AllowPlexTokenDetailed(context.Background(), "friend-token")
	if !d.Allowed || d.Unavailable || d.CacheHit {
		t.Fatalf("after PMS recovers: got %+v, want a fresh allow", d)
	}
	if calls.Load() != 2 {
		t.Fatalf("PMS calls = %d, want 2 (the 503 must not be cached)", calls.Load())
	}
}

func TestPlexTokenAuthorizer_RefusalIsCached(t *testing.T) {
	var status, calls atomic.Int32
	status.Store(http.StatusUnauthorized)
	pms := pmsWithStatus(t, &status, &calls)
	a, err := NewPlexTokenAuthorizer(pms.URL, "owner-token", nil)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		d := a.AllowPlexTokenDetailed(context.Background(), "stranger-token")
		if d.Allowed || d.Unavailable {
			t.Fatalf("call %d: got %+v, want a definite refusal", i+1, d)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("PMS calls = %d, want 1 (a 401 is cached)", calls.Load())
	}
}

func TestPlexTokenAuthorizer_RecentlyAllowedTokenSurvivesPMSOutage(t *testing.T) {
	var status, calls atomic.Int32
	status.Store(http.StatusOK)
	pms := pmsWithStatus(t, &status, &calls)
	a, err := NewPlexTokenAuthorizer(pms.URL, "owner-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	a.ttl = 0 // ask PMS on every call
	a.retryAfter = 0

	if d := a.AllowPlexTokenDetailed(context.Background(), "friend-token"); !d.Allowed {
		t.Fatalf("PMS 200: got %+v, want allowed", d)
	}

	// PMS restarting: unreachable, then answering 503.
	pms.Close()
	d := a.AllowPlexTokenDetailed(context.Background(), "friend-token")
	if !d.Allowed || !d.Unavailable {
		t.Fatalf("PMS unreachable after a recent allow: got %+v, want allowed and unavailable", d)
	}
	if d := a.AllowPlexTokenDetailed(context.Background(), "other-token"); d.Allowed || !d.Unavailable {
		t.Fatalf("PMS unreachable, token never allowed: got %+v, want refused and unavailable", d)
	}

	// Past the stale window the outage no longer lets it through.
	a.mu.Lock()
	a.lastAllowed["friend-token"] = time.Now().Add(-a.staleFor - time.Minute)
	a.mu.Unlock()
	if d := a.AllowPlexTokenDetailed(context.Background(), "friend-token"); d.Allowed || !d.Unavailable {
		t.Fatalf("PMS unreachable, allow too old: got %+v, want refused and unavailable", d)
	}
}

func TestPlexTokenAuthorizer_DefiniteRefusalEndsStaleAllow(t *testing.T) {
	var status, calls atomic.Int32
	status.Store(http.StatusOK)
	pms := pmsWithStatus(t, &status, &calls)
	a, err := NewPlexTokenAuthorizer(pms.URL, "owner-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	a.ttl = 0
	a.retryAfter = 0

	if d := a.AllowPlexTokenDetailed(context.Background(), "friend-token"); !d.Allowed {
		t.Fatalf("PMS 200: got %+v, want allowed", d)
	}
	status.Store(http.StatusUnauthorized) // the share was removed
	if d := a.AllowPlexTokenDetailed(context.Background(), "friend-token"); d.Allowed || d.Unavailable {
		t.Fatalf("PMS 401: got %+v, want a definite refusal", d)
	}
	status.Store(http.StatusBadGateway)
	if d := a.AllowPlexTokenDetailed(context.Background(), "friend-token"); d.Allowed {
		t.Fatalf("PMS 502 after a 401: got %+v, want refused", d)
	}
}

// unavailableAuthorizer reports that PMS couldn't answer until available is set.
type unavailableAuthorizer struct {
	available atomic.Bool
	calls     atomic.Int32
}

func (a *unavailableAuthorizer) AllowPlexToken(ctx context.Context, token string) bool {
	return a.AllowPlexTokenDetailed(ctx, token).Allowed
}

func (a *unavailableAuthorizer) AllowPlexTokenDetailed(_ context.Context, token string) AuthorizationDecision {
	a.calls.Add(1)
	if !a.available.Load() {
		return AuthorizationDecision{Unavailable: true}
	}
	return AuthorizationDecision{Allowed: token == "shared-user-token"}
}

func TestProxy_PMSUnavailableIsNotABadAttempt(t *testing.T) {
	var logBuf bytes.Buffer
	var gotToken atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken.Store(r.URL.Query().Get("X-Plex-Token"))
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<MediaContainer/>`))
	}))
	defer up.Close()

	authorizer := &unavailableAuthorizer{}
	proxy, err := New(Config{
		Upstream:            up.URL,
		Token:               "label-token",
		OwnerToken:          "owner-token",
		ElevateLiveTV:       true,
		TokenAuthorizer:     authorizer,
		Logger:              log.New(&logBuf, "", 0),
		AbuseBlockThreshold: 2,
		AbuseBlockWindow:    time.Minute,
		AbuseBlockDuration:  time.Hour,
		BadAuthCooldown:     time.Hour,
	})
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/livetv/dvrs?X-Plex-Token=shared-user-token", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Forwarded-For", "203.0.113.77")
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
		return rec
	}

	// More PMS outages than the block threshold: none may block the source.
	for i := 0; i < 4; i++ {
		if rec := send(); rec.Code != http.StatusOK {
			t.Fatalf("request %d during PMS outage: status=%d, want 200 (passed through, not blocked)", i+1, rec.Code)
		}
		if got, _ := gotToken.Load().(string); got != "shared-user-token" {
			t.Fatalf("request %d during PMS outage was elevated (token %q)", i+1, got)
		}
	}
	if authorizer.calls.Load() != 4 {
		t.Fatalf("authorizer calls = %d, want 4 (no cooldown skipping the check)", authorizer.calls.Load())
	}

	// PMS is back: the same source and token are elevated at once.
	authorizer.available.Store(true)
	if rec := send(); rec.Code != http.StatusOK {
		t.Fatalf("after PMS recovered: status=%d, want 200", rec.Code)
	}
	if got, _ := gotToken.Load().(string); got != "owner-token" {
		t.Fatalf("after PMS recovered the request was not elevated (token %q)", got)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "outcome=deny_auth_unavailable") {
		t.Fatalf("expected deny_auth_unavailable audit lines, got %s", logged)
	}
	for _, unwanted := range []string{"outcome=bad_actor_blocked", "outcome=blocked_bad_actor", "outcome=deny_auth_cooldown", "outcome=deny_unauthorized_token"} {
		if strings.Contains(logged, unwanted) {
			t.Fatalf("PMS outage produced %s: %s", unwanted, logged)
		}
	}
}
