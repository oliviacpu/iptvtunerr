package plexlabelproxy

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
)

// tokenlessSegmentToken tunes a channel as a shared user, then fetches a
// segment of that session without a token, and returns the token the segment
// request reached Plex with.
func tokenlessSegmentToken(t *testing.T, noRecovery bool) string {
	t.Helper()
	var gotToken string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.URL.Query().Get("X-Plex-Token")
		w.Header().Set("Content-Type", "video/mp2t")
		_, _ = w.Write([]byte("segment"))
	}))
	defer up.Close()

	proxy, err := New(Config{
		Upstream:               up.URL,
		Token:                  "label-token",
		OwnerToken:             "owner-token",
		ElevateLiveTV:          true,
		NeutralizeOwnerHistory: true,
		NoSessionTokenRecovery: noRecovery,
		TokenAuthorizer:        staticTokenAuthorizer{"shared-user-token": true},
		Logger:                 log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}

	start := httptest.NewRequest(http.MethodPost, "/livetv/dvrs/123/channels/c7pt/tune?X-Plex-Token=shared-user-token", nil)
	start.Header.Set("X-Plex-Client-Identifier", "client-1")
	proxy.ServeHTTP(httptest.NewRecorder(), start)

	proxy.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/livetv/sessions/session-1/client-1/00000.ts", nil))
	return gotToken
}

func TestProxy_TokenlessSegmentRecoveredByDefault(t *testing.T) {
	if got := tokenlessSegmentToken(t, false); got != "owner-token" {
		t.Fatalf("tokenless segment reached Plex with %q, want the owner token", got)
	}
}

func TestProxy_NoSessionTokenRecoveryNeverElevatesTokenless(t *testing.T) {
	if got := tokenlessSegmentToken(t, true); got != "" {
		t.Fatalf("tokenless segment reached Plex with %q, want no token", got)
	}
}
