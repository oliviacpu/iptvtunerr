package plexlabelproxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A shared user must not be able to borrow the owner token for library content
// by mentioning a Live TV path somewhere other than the path a parameter names.
func TestIsLiveTVRequest_LiveTVTextOutsideThePathIsNotElevated(t *testing.T) {
	for name, target := range map[string]string{
		"query string":        "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F123%3Fx%3D%2Flivetv%2F",
		"fragment":            "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F123%23%2Flivetv%2F",
		"dot-dot":             "/video/:/transcode/universal/start.m3u8?path=%2Flivetv%2F..%2Flibrary%2Fmetadata%2F123",
		"double-escaped":      "/video/:/transcode/universal/start.m3u8?path=%252Flivetv%252F..%252F..%252Flibrary%252Fmetadata%252F123",
		"provider dot-dot":    "/video/:/transcode/universal/start.m3u8?path=%2Ftv.plex.providers.epg.xmltv%3A1%2F..%2Flibrary%2Fmetadata%2F123",
		"server URI query":    "/video/:/transcode/universal/start.m3u8?path=server%3A%2F%2Fabc%2Fcom.plexapp.plugins.library%2Flibrary%2Fmetadata%2F1%3Fx%3D%2Flivetv%2F",
		"live TV word in key": "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2Flivetv",
	} {
		if IsLiveTVRequest(httptest.NewRequest(http.MethodGet, target, nil)) {
			t.Errorf("%s: %s must not be elevated", name, target)
		}
	}

	posts := map[string]*http.Request{
		"play queue URI query": httptest.NewRequest(http.MethodPost,
			"/playQueues?uri=server%3A%2F%2Fabc%2Fcom.plexapp.plugins.library%2Flibrary%2Fmetadata%2F123%3Fx%3D%2Flivetv%2F", nil),
		"form body, other field": formRequest(http.MethodPost, "/playQueues", "uri=%2Flibrary%2Fmetadata%2F123&note=%2Flivetv%2Fx"),
		"JSON body, other field": jsonRequest(http.MethodPost, "/playQueues", `{"uri":"/library/metadata/123","note":"/livetv/x"}`),
		"subscription, other field": formRequest(http.MethodPost, "/media/subscriptions",
			"prefs%5Bnote%5D=%2Flivetv%2Fsessions%2Fabc"),
	}
	for name, req := range posts {
		if IsLiveTVRequest(req) {
			t.Errorf("%s: %s must not be elevated", name, req.URL.String())
		}
	}
}

func TestIsLiveTVRequest_RealLiveTVTargetsStillElevated(t *testing.T) {
	gets := []string{
		"/video/:/transcode/universal/start.m3u8?path=%2Flivetv%2Fsessions%2Fabc%2Findex.m3u8",
		"/video/:/transcode/universal/start.m3u8?path=http%3A%2F%2F127.0.0.1%3A32400%2Flivetv%2Fsessions%2Fabc%2Findex.m3u8",
		"/media/subscriptions/template?guid=tv.plex.xmltv%3A%2F%2Fmovie%2FLive%253A%2520NBA",
	}
	for _, target := range gets {
		if !IsLiveTVRequest(httptest.NewRequest(http.MethodGet, target, nil)) {
			t.Errorf("%s should be elevated", target)
		}
	}

	posts := map[string]*http.Request{
		"provider in server URI": httptest.NewRequest(http.MethodPost,
			"/playQueues?uri=server%3A%2F%2Fabc%2Ftv.plex.providers.epg.xmltv%3A4%2Fmetadata%2Fxyz", nil),
		"live TV path in server URI": httptest.NewRequest(http.MethodPost,
			"/playQueues?uri=server%3A%2F%2Fabc%2Fcom.plexapp.plugins.library%2Flivetv%2Fsessions%2Fabc", nil),
		"JSON body": jsonRequest(http.MethodPost, "/playQueues", `{"uri":"/livetv/sessions/abc/index.m3u8"}`),
		"subscription hint key": httptest.NewRequest(http.MethodPost,
			"/media/subscriptions?hints%5Bkey%5D=%2Ftv.plex.providers.epg.xmltv%3A123%2Fgrid%2Fmetadata%2Fabc", nil),
		"subscription provider key in form body": formRequest(http.MethodPost, "/media/subscriptions",
			"hints%5BratingKey%5D=tv.plex.providers.epg.xmltv%3A123%2Fmetadata%2Fabc"),
	}
	for name, req := range posts {
		if !IsLiveTVRequest(req) {
			t.Errorf("%s: %s should be elevated", name, req.URL.String())
		}
	}
}

func TestProxy_LibraryTranscodeMentioningLiveTVKeepsUserToken(t *testing.T) {
	var got string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("X-Plex-Token")
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	proxy, err := New(Config{Upstream: up.URL, Token: "label-token", OwnerToken: "owner-token", ElevateLiveTV: true})
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	proxy.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet,
		"/video/:/transcode/universal/start.m3u8?X-Plex-Token=user-token&path=%2Flibrary%2Fmetadata%2F123%3Fx%3D%2Flivetv%2F", nil))

	if got != "user-token" {
		t.Fatalf("library transcode reached Plex with token %q, want the user's", got)
	}
}

func formRequest(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func jsonRequest(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}
