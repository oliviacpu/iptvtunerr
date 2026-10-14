package plexlabelproxy

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// echoUpstream is a Plex that puts the token each request arrived with into
// its response, the way URLs Plex builds from a request could.
func echoUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("X-Plex-Token")
		switch {
		case strings.HasSuffix(r.URL.Path, "/redirect"):
			w.Header().Set("Location", "/livetv/sessions/abc/index.m3u8?X-Plex-Token="+token)
			w.WriteHeader(http.StatusFound)
		case strings.HasSuffix(r.URL.Path, ".m3u8"):
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\nsegment0.ts?X-Plex-Token="+token+"\n")
		case strings.HasSuffix(r.URL.Path, "/gzip"):
			w.Header().Set("Content-Type", "application/xml")
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			_, _ = io.WriteString(gz, `<MediaContainer key="/livetv/x?X-Plex-Token=`+token+`"/>`)
			_ = gz.Close()
		case strings.HasSuffix(r.URL.Path, ".ts"):
			w.Header().Set("Content-Type", "video/mp2t")
			_, _ = io.WriteString(w, "binary "+token)
		default:
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, `<MediaContainer key="/livetv/dvrs?X-Plex-Token=`+token+`"/>`)
		}
	}))
}

func scrubProxy(t *testing.T, upstream string) *Proxy {
	t.Helper()
	proxy, err := New(Config{Upstream: upstream, Token: "label-token", OwnerToken: "owner-token", ElevateLiveTV: true})
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	return proxy
}

func responseText(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	body := rec.Body.Bytes()
	if rec.Header().Get("Content-Encoding") == "gzip" {
		gr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("gzip: %v", err)
		}
		body, _ = io.ReadAll(gr)
	}
	var headers strings.Builder
	for name, values := range rec.Header() {
		headers.WriteString(name + ": " + strings.Join(values, ",") + "\n")
	}
	return headers.String() + string(body)
}

func TestProxy_ElevatedResponsesNeverCarryTheOwnerToken(t *testing.T) {
	up := echoUpstream(t)
	defer up.Close()
	proxy := scrubProxy(t, up.URL)

	for _, target := range []string{
		"/livetv/dvrs",
		"/livetv/sessions/abc/redirect",
		"/livetv/sessions/abc/index.m3u8",
		"/livetv/sessions/abc/gzip",
	} {
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target+"?X-Plex-Token=user-token", nil))
		text := responseText(t, rec)
		if strings.Contains(text, "owner-token") {
			t.Errorf("%s: the response carries the owner token:\n%s", target, text)
		}
		if !strings.Contains(text, "user-token") {
			t.Errorf("%s: the echoed URL should carry the user's token instead:\n%s", target, text)
		}
	}
}

func TestProxy_ScrubLeavesUnelevatedAndMediaResponsesAlone(t *testing.T) {
	up := echoUpstream(t)
	defer up.Close()
	proxy := scrubProxy(t, up.URL)

	// A library request keeps the user token end to end
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/library/sections?X-Plex-Token=user-token", nil))
	if got := rec.Body.String(); !strings.Contains(got, "user-token") {
		t.Errorf("library response changed: %s", got)
	}

	// The owner's own requests have nothing to hide
	rec = httptest.NewRecorder()
	proxy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/livetv/dvrs?X-Plex-Token=owner-token", nil))
	if got := rec.Body.String(); !strings.Contains(got, "owner-token") {
		t.Errorf("owner's response changed: %s", got)
	}
}
