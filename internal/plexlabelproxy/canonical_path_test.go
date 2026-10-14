package plexlabelproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Elevation is decided on the request path as written. A path Plex would
// resolve to somewhere else must never carry the owner token there.
func TestIsLiveTVRequest_NonCanonicalPathsAreNotElevated(t *testing.T) {
	for _, target := range []string{
		"/livetv/../:/prefs",
		"/livetv/./dvrs",
		"/livetv/%2e%2e/library/sections",
		"/livetv/..%2flibrary/sections",
		"/livetv/..%5clibrary/sections",
		"/livetv//dvrs",
		"/media/grabbers/../../library/sections/1/refresh",
		"/media/subscriptions/../../:/prefs",
		"/tv.plex.providers.epg.xmltv:1/../../library/sections",
		"/video/:/transcode/../../library/sections?path=%2Flivetv%2Fsessions%2Fabc",
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			req := httptest.NewRequest(method, "http://plex"+target, nil)
			if IsLiveTVRequest(req) || IsLiveTVDiscoveryRequest(req) || IsLiveTVStreamRequest(req) {
				t.Errorf("%s %s must not be elevated", method, target)
			}
		}
	}
}

func TestIsLiveTVRequest_CanonicalLiveTVPathsStillElevated(t *testing.T) {
	for _, target := range []string{
		"/livetv/dvrs",
		"/livetv/dvrs/",
		"/livetv/sessions/abc/client-1/index.m3u8",
		"/media/grabbers/devices",
		"/tv.plex.providers.epg.xmltv:1/grid",
	} {
		if !IsLiveTVRequest(httptest.NewRequest(http.MethodGet, "http://plex"+target, nil)) {
			t.Errorf("%s should be elevated", target)
		}
	}
}

func TestProxy_DotSegmentPathKeepsUserToken(t *testing.T) {
	got := proxyTokens(t, Config{}, []*http.Request{
		httptest.NewRequest(http.MethodGet, "http://plex/livetv/../library/sections?X-Plex-Token=user-token", nil),
		httptest.NewRequest(http.MethodGet, "http://plex/livetv/dvrs?X-Plex-Token=user-token", nil),
	})
	if got[0] != "user-token" {
		t.Errorf("dot-segment path reached Plex with %q, want the user's token", got[0])
	}
	if got[1] != "owner-token" {
		t.Errorf("Live TV path reached Plex with %q, want the owner token", got[1])
	}
}
