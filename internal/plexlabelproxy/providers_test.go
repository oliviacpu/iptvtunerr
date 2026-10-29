package plexlabelproxy

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const liveProviderID = "tv.plex.providers.epg.xmltv:4"

// providersFixture is a PMS answering /media/providers per token with
// testdata/mediaproviders: owner-token gets the owner's list (libraries 3,
// 4, 5, 7, 8 and 9, playlists, Live TV provider 4, the owner's account),
// friend-token a shared user's (libraries 3, 4 and 5, no Live TV). JSON when
// asked for, XML otherwise, gzipped when gzip is set. Anything else gets an
// empty container.
type providersFixture struct {
	pms  *httptest.Server
	gzip atomic.Bool
	// friendAnswer, when set, replaces the friend's answer.
	friendAnswer atomic.Pointer[func(http.ResponseWriter)]
	logs         syncBuffer

	mu    sync.Mutex
	calls []providersCall
}

type providersCall struct {
	path, token, query, accept string
	conditional                bool
}

func newProvidersFixture(t *testing.T) *providersFixture {
	t.Helper()
	f := &providersFixture{}
	f.pms = httptest.NewServer(http.HandlerFunc(f.servePMS))
	t.Cleanup(f.pms.Close)
	t.Cleanup(func() {
		logs := f.logs.String()
		for _, token := range []string{"friend-token", "owner-token", "label-token"} {
			if strings.Contains(logs, token) {
				t.Errorf("logs carry the raw token %q:\n%s", token, logs)
			}
		}
	})
	return f
}

func (f *providersFixture) servePMS(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("X-Plex-Token")
	if token == "" {
		token = r.Header.Get("X-Plex-Token")
	}
	q := r.URL.Query()
	q.Del("X-Plex-Token")
	f.mu.Lock()
	f.calls = append(f.calls, providersCall{
		path:        r.URL.Path,
		token:       token,
		query:       q.Encode(),
		accept:      r.Header.Get("Accept"),
		conditional: r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "",
	})
	f.mu.Unlock()
	if r.URL.Path != "/media/providers" {
		w.Header().Set("Content-Type", "text/xml;charset=utf-8")
		_, _ = w.Write([]byte(`<MediaContainer size="0"/>`))
		return
	}
	var name string
	switch token {
	case "owner-token":
		name = "owner"
	case "friend-token":
		if answer := f.friendAnswer.Load(); answer != nil {
			(*answer)(w)
			return
		}
		name = "friend"
	default:
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Header.Get("If-None-Match") == `"providers-1"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	asJSON := strings.Contains(r.Header.Get("Accept"), "json")
	body := providersFixtureBody(name, asJSON)
	if asJSON {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "text/xml;charset=utf-8")
	}
	w.Header().Set("ETag", `"providers-1"`)
	w.Header().Set("X-Plex-Content-Original-Length", strconv.Itoa(len(body)))
	if f.gzip.Load() && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		body = gzipBytes(body)
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("X-Plex-Content-Compressed-Length", strconv.Itoa(len(body)))
	}
	_, _ = w.Write(body)
}

func providersFixtureBody(name string, asJSON bool) []byte {
	ext := ".xml"
	if asJSON {
		ext = ".json"
	}
	b, err := os.ReadFile(filepath.Join("testdata", "mediaproviders", name+ext))
	if err != nil {
		panic(err)
	}
	return b
}

func (f *providersFixture) setFriendAnswer(answer func(http.ResponseWriter)) {
	f.friendAnswer.Store(&answer)
}

func (f *providersFixture) providerCalls() []providersCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []providersCall
	for _, c := range f.calls {
		if c.path == "/media/providers" {
			out = append(out, c)
		}
	}
	return out
}

func (f *providersFixture) proxy(t *testing.T, edit func(*Config)) *Proxy {
	t.Helper()
	cfg := Config{
		Upstream:        f.pms.URL,
		Token:           "label-token",
		OwnerToken:      "owner-token",
		ElevateLiveTV:   true,
		TokenAuthorizer: staticTokenAuthorizer{"friend-token": true},
		Logger:          log.New(&f.logs, "", 0),
	}
	if edit != nil {
		edit(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	return p
}

// providersView is what a /media/providers answer tells a client.
type providersView struct {
	attrs     map[string]string
	providers []string
	// sections are the ids of the library provider's content directories.
	sections []string
	// liveFeatures are the Live TV provider's feature types.
	liveFeatures []string
	liveTitle    string
}

func (v providersView) flag(name string) bool {
	return v.attrs[name] == "1" || v.attrs[name] == "true"
}

type providersDoc struct {
	MediaProvider []struct {
		Identifier string `xml:"identifier,attr" json:"identifier"`
		Title      string `xml:"title,attr" json:"title"`
		Feature    []struct {
			Type      string `xml:"type,attr" json:"type"`
			Directory []struct {
				ID string `xml:"id,attr" json:"id"`
			} `xml:"Directory" json:"Directory"`
		} `xml:"Feature" json:"Feature"`
	} `xml:"MediaProvider" json:"MediaProvider"`
}

func viewProviders(t *testing.T, body []byte, format string) providersView {
	t.Helper()
	var doc providersDoc
	v := providersView{attrs: map[string]string{}}
	if format == "json" {
		var outer struct {
			MediaContainer json.RawMessage
		}
		if err := json.Unmarshal(body, &outer); err != nil {
			t.Fatalf("decode JSON: %v\n%s", err, body)
		}
		if err := json.Unmarshal(outer.MediaContainer, &doc); err != nil {
			t.Fatalf("decode JSON: %v\n%s", err, body)
		}
		var attrs map[string]any
		_ = json.Unmarshal(outer.MediaContainer, &attrs)
		for k, a := range attrs {
			switch a.(type) {
			case []any, map[string]any:
			default:
				v.attrs[k] = fmt.Sprint(a)
			}
		}
	} else {
		var root struct {
			XMLName xml.Name   `xml:"MediaContainer"`
			Attrs   []xml.Attr `xml:",any,attr"`
			providersDoc
		}
		if err := xml.Unmarshal(body, &root); err != nil {
			t.Fatalf("decode XML: %v\n%s", err, body)
		}
		for _, a := range root.Attrs {
			v.attrs[a.Name.Local] = a.Value
		}
		doc = root.providersDoc
	}
	for _, mp := range doc.MediaProvider {
		v.providers = append(v.providers, mp.Identifier)
		for _, feat := range mp.Feature {
			switch {
			case mp.Identifier == "com.plexapp.plugins.library" && feat.Type == "content":
				for _, d := range feat.Directory {
					if d.ID != "" {
						v.sections = append(v.sections, d.ID)
					}
				}
			case mp.Identifier == liveProviderID:
				v.liveFeatures = append(v.liveFeatures, feat.Type)
			}
		}
		if mp.Identifier == liveProviderID {
			v.liveTitle = mp.Title
		}
	}
	return v
}

var ownerLiveFeatures = []string{"content", "match", "metadata", "search", "subscribe", "grid", "settings"}

func TestOwnProviders_SharedUserGetsTheirOwnListPlusLiveTV(t *testing.T) {
	for _, format := range []string{"json", "xml"} {
		for _, gz := range []bool{false, true} {
			for _, inQuery := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/gzip=%v/token_in_query=%v", format, gz, inQuery), func(t *testing.T) {
					f := newProvidersFixture(t)
					f.gzip.Store(gz)
					p := f.proxy(t, nil)

					target, token := "/media/providers?includePreferences=1", "friend-token"
					if inQuery {
						target, token = target+"&X-Plex-Token=friend-token", ""
					}
					rec := hideServe(p, http.MethodGet, target, token, format)
					if rec.Code != http.StatusOK {
						t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
					}
					if got := rec.Header().Get("Content-Encoding") == "gzip"; got != gz {
						t.Fatalf("gzipped=%v, want %v", got, gz)
					}
					body := hideBody(t, rec)
					v := viewProviders(t, body, format)

					if want := []string{"com.plexapp.plugins.library", liveProviderID}; !slices.Equal(v.providers, want) {
						t.Fatalf("providers=%q, want %q\n%s", v.providers, want, body)
					}
					if want := []string{"3", "4", "5"}; !slices.Equal(v.sections, want) {
						t.Fatalf("library sections=%q, want the friend's own %q\n%s", v.sections, want, body)
					}
					if !slices.Equal(v.liveFeatures, ownerLiveFeatures) {
						t.Fatalf("Live TV features=%q, want %q", v.liveFeatures, ownerLiveFeatures)
					}
					if _, ok := v.attrs["myPlexUsername"]; ok || bytes.Contains(body, []byte("owner@example.com")) {
						t.Fatalf("the owner's account is in the friend's answer:\n%s", body)
					}
					// sections above has no "playlists": the owner's Playlists directory is gone too.
					if bytes.Contains(body, []byte("Commercials")) || bytes.Contains(body, []byte("library/sections/9")) {
						t.Fatalf("an unshared library is in the friend's answer:\n%s", body)
					}
					if v.flag("allowMediaDeletion") || v.flag("allowSharing") {
						t.Fatalf("owner-level container flags in the friend's answer: %v", v.attrs)
					}
					if v.attrs["size"] != "2" {
						t.Fatalf("size=%q, want 2", v.attrs["size"])
					}
					if !v.flag("allowTuners") {
						t.Fatalf("allowTuners=%q, want the entitlement hint", v.attrs["allowTuners"])
					}
					if cc := rec.Header().Get("Cache-Control"); cc != "private, no-store" {
						t.Fatalf("Cache-Control=%q", cc)
					}
					for _, h := range []string{"ETag", "X-Plex-Content-Original-Length", "X-Plex-Content-Compressed-Length"} {
						if rec.Header().Get(h) != "" {
							t.Fatalf("%s is the owner body's: %q", h, rec.Header().Get(h))
						}
					}

					calls := f.providerCalls()
					if len(calls) != 2 {
						t.Fatalf("PMS calls=%+v, want the elevated one and the friend's own", calls)
					}
					wantAccept := "application/" + format
					if c := calls[0]; c.token != "owner-token" || c.query != "includePreferences=1" || c.accept != wantAccept {
						t.Fatalf("elevated call=%+v", c)
					}
					if c := calls[1]; c.token != "friend-token" || c.query != "includePreferences=1" || c.accept != wantAccept {
						t.Fatalf("own list call=%+v, want the friend's token, query and Accept", c)
					}
					wantLog := "plexlabelproxy_providers: outcome=own_list token_fp=" + tokenFingerprint("friend-token") + " providers=2 live_tv=1"
					if !strings.Contains(f.logs.String(), wantLog) {
						t.Fatalf("missing %q in logs:\n%s", wantLog, f.logs.String())
					}
				})
			}
		}
	}
}

func TestOwnProviders_LogsOnceEveryFewMinutesPerToken(t *testing.T) {
	f := newProvidersFixture(t)
	p := f.proxy(t, nil)
	for i := 0; i < 3; i++ {
		if rec := hideServe(p, http.MethodGet, "/media/providers", "friend-token", "xml"); rec.Code != http.StatusOK {
			t.Fatalf("status=%d", rec.Code)
		}
	}
	if n := strings.Count(f.logs.String(), "outcome=own_list"); n != 1 {
		t.Fatalf("own_list lines=%d, want 1:\n%s", n, f.logs.String())
	}
	if n := len(f.providerCalls()); n != 6 {
		t.Fatalf("PMS calls=%d, want 2 per request", n)
	}
}

func TestOwnProviders_LabelsTheGraftedLiveTVProvider(t *testing.T) {
	f := newProvidersFixture(t)
	p := f.proxy(t, func(c *Config) {
		c.Labels = StaticLabelSource(map[string]string{liveProviderID: "Example Live"})
	})
	rec := hideServe(p, http.MethodGet, "/media/providers", "friend-token", "xml")
	body := hideBody(t, rec)
	v := viewProviders(t, body, "xml")
	if v.liveTitle != "Example Live" || !bytes.Contains(body, []byte(`friendlyName="Example Live"`)) {
		t.Fatalf("Live TV provider not relabelled (title %q):\n%s", v.liveTitle, body)
	}
	if want := []string{"3", "4", "5"}; !slices.Equal(v.sections, want) {
		t.Fatalf("library sections=%q, want %q", v.sections, want)
	}
}

func TestOwnProviders_OwnerKeepsTheirAnswer(t *testing.T) {
	for _, format := range []string{"json", "xml"} {
		t.Run(format, func(t *testing.T) {
			f := newProvidersFixture(t)
			p := f.proxy(t, nil)
			rec := hideServe(p, http.MethodGet, "/media/providers?includePreferences=1", "owner-token", format)
			if got, want := hideBody(t, rec), providersFixtureBody("owner", format == "json"); !bytes.Equal(got, want) {
				t.Fatalf("owner body changed:\n%s", got)
			}
			if rec.Header().Get("ETag") == "" {
				t.Fatal("the owner's answer lost its ETag")
			}
			if calls := f.providerCalls(); len(calls) != 1 || calls[0].token != "owner-token" {
				t.Fatalf("PMS calls=%+v, want the owner's one", calls)
			}
			if strings.Contains(f.logs.String(), "plexlabelproxy_providers:") {
				t.Fatalf("owner request logged:\n%s", f.logs.String())
			}
		})
	}
}

func TestOwnProviders_NotWithElevateAll(t *testing.T) {
	f := newProvidersFixture(t)
	// Everything is the owner's to a shared user then: libraries included.
	p := f.proxy(t, func(c *Config) { c.ElevateAll = true })
	rec := hideServe(p, http.MethodGet, "/media/providers", "friend-token", "xml")
	if v := viewProviders(t, hideBody(t, rec), "xml"); len(v.sections) != 7 {
		t.Fatalf("sections=%q, want the owner's", v.sections)
	}
	if calls := f.providerCalls(); len(calls) != 1 {
		t.Fatalf("PMS calls=%+v, want one", calls)
	}
}

func TestOwnProviders_FallsBackToTheElevatedAnswer(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		answer       func(http.ResponseWriter)
	}{
		{"status", "reason=status_500", func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) }},
		{"content_type", "reason=content_type_mismatch", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(providersFixtureBody("friend", true))
		}},
		{"unparsed", "reason=unparsed", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/xml;charset=utf-8")
			_, _ = w.Write([]byte(`<Error code="1">not a container</Error>`))
		}},
		{"timeout", "reason=timeout", func(w http.ResponseWriter) {
			time.Sleep(300 * time.Millisecond)
			w.Header().Set("Content-Type", "text/xml;charset=utf-8")
			_, _ = w.Write(providersFixtureBody("friend", false))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProvidersFixture(t)
			f.setFriendAnswer(tc.answer)
			p := f.proxy(t, nil)
			p.ownProviders.timeout = 100 * time.Millisecond
			rec := hideServe(p, http.MethodGet, "/media/providers", "friend-token", "xml")
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d", rec.Code)
			}
			// Today's elevated answer: Live TV keeps working.
			v := viewProviders(t, hideBody(t, rec), "xml")
			if !slices.Contains(v.providers, liveProviderID) || len(v.sections) != 7 {
				t.Fatalf("not the elevated answer: %+v", v)
			}
			wantLog := "plexlabelproxy_providers: outcome=fallback token_fp=" + tokenFingerprint("friend-token") + " " + tc.reason
			if !strings.Contains(f.logs.String(), wantLog) {
				t.Fatalf("missing %q in logs:\n%s", wantLog, f.logs.String())
			}
		})
	}
}

func TestOwnProviders_ConditionalRequestGetsTheWholeAnswer(t *testing.T) {
	f := newProvidersFixture(t)
	p := f.proxy(t, nil)
	// An ETag the client kept from an owner's list served before.
	rec := hideServe(p, http.MethodGet, "/media/providers", "friend-token", "xml", "If-None-Match", `"providers-1"`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want the whole answer", rec.Code)
	}
	if v := viewProviders(t, hideBody(t, rec), "xml"); !slices.Equal(v.sections, []string{"3", "4", "5"}) {
		t.Fatalf("sections=%q", v.sections)
	}
	for _, c := range f.providerCalls() {
		if c.conditional {
			t.Fatalf("a conditional request reached PMS: %+v", c)
		}
	}
	// The owner's own conditional requests are theirs to make.
	if rec := hideServe(p, http.MethodGet, "/media/providers", "owner-token", "xml", "If-None-Match", `"providers-1"`); rec.Code != http.StatusNotModified {
		t.Fatalf("owner status=%d, want 304", rec.Code)
	}
}

func TestOwnProviders_NeverAsksPMSWithoutAToken(t *testing.T) {
	f := newProvidersFixture(t)
	p := f.proxy(t, func(c *Config) { c.NeutralizeOwnerHistory = true })
	// A tune tracks the friend's client; a tokenless request from it is
	// elevated with the friend's session.
	start := httptest.NewRequest(http.MethodPost, "/livetv/dvrs/4/channels/C1.49.tunarr.com/tune?X-Plex-Token=friend-token", nil)
	start.RemoteAddr = "127.0.0.1:12345"
	start.Header.Set("X-Plex-Client-Identifier", "client-1")
	p.ServeHTTP(httptest.NewRecorder(), start)

	rec := hideServe(p, http.MethodGet, "/media/providers", "", "xml", "X-Plex-Client-Identifier", "client-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	calls := f.providerCalls()
	if len(calls) != 1 || calls[0].token != "owner-token" {
		t.Fatalf("PMS calls=%+v, want only the elevated one", calls)
	}
	if !strings.Contains(f.logs.String(), "plexlabelproxy_providers: outcome=fallback token_fp=missing reason=no_caller_token") {
		t.Fatalf("missing the no_caller_token fallback:\n%s", f.logs.String())
	}
}

func TestGraftLiveProviders(t *testing.T) {
	const ownXML = `<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer size="2" allowTuners="0">
<MediaProvider identifier="com.plexapp.plugins.library" title="Library"/>
<MediaProvider identifier="tv.plex.providers.epg.xmltv:4" title="Stale"/>
</MediaContainer>`
	const elevatedXML = `<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer size="3" myPlexUsername="owner@example.com">
<MediaProvider identifier="com.plexapp.plugins.library" title="Owner Library"/>
<MediaProvider identifier="tv.plex.providers.epg.xmltv:4" title="Live TV &amp; DVR"><Feature type="content"/></MediaProvider>
<MediaProvider identifier="tv.plex.providers.epg.xmltv:5" title="Second DVR"/>
</MediaContainer>`
	const ownJSON = `{"MediaContainer":{"size":2,"allowTuners":false,"title":"<a&b>","MediaProvider":[{"identifier":"com.plexapp.plugins.library","title":"Library"},{"identifier":"tv.plex.providers.epg.xmltv:4","title":"Stale"}]}}`
	const elevatedJSON = `{"MediaContainer":{"size":3,"myPlexUsername":"owner@example.com","MediaProvider":[{"identifier":"com.plexapp.plugins.library","title":"Owner Library"},{"identifier":"tv.plex.providers.epg.xmltv:4","title":"Live TV & DVR","Feature":[{"type":"content"}]},{"identifier":"tv.plex.providers.epg.xmltv:5","title":"Second DVR"}]}}`

	for _, tc := range []struct {
		format, own, elevated string
	}{
		{"xml", ownXML, elevatedXML},
		{"json", ownJSON, elevatedJSON},
	} {
		t.Run(tc.format, func(t *testing.T) {
			out, providers, live, err := graftLiveProviders([]byte(tc.own), []byte(tc.elevated), tc.format == "json")
			if err != nil {
				t.Fatalf("graft: %v", err)
			}
			if providers != 3 || live != 2 {
				t.Fatalf("providers=%d live=%d, want 3 and 2", providers, live)
			}
			var doc providersDoc
			if tc.format == "json" {
				var outer struct{ MediaContainer providersDoc }
				if err := json.Unmarshal(out, &outer); err != nil {
					t.Fatalf("decode: %v\n%s", err, out)
				}
				doc = outer.MediaContainer
				if !bytes.Contains(out, []byte(`"title":"<a&b>"`)) || !bytes.Contains(out, []byte(`"allowTuners":false`)) {
					t.Fatalf("the caller's container changed:\n%s", out)
				}
			} else {
				if err := xml.Unmarshal(out, &doc); err != nil {
					t.Fatalf("decode: %v\n%s", err, out)
				}
				if !bytes.HasPrefix(out, []byte(`<?xml version="1.0" encoding="UTF-8"?>`)) {
					t.Fatalf("XML declaration lost:\n%s", out)
				}
			}
			var got []string
			for _, mp := range doc.MediaProvider {
				got = append(got, mp.Identifier+"="+mp.Title)
			}
			// Same-identifier provider replaced in place; the new one appended.
			want := []string{"com.plexapp.plugins.library=Library", "tv.plex.providers.epg.xmltv:4=Live TV & DVR", "tv.plex.providers.epg.xmltv:5=Second DVR"}
			if !slices.Equal(got, want) {
				t.Fatalf("providers=%q, want %q\n%s", got, want, out)
			}
			if len(doc.MediaProvider[1].Feature) != 1 {
				t.Fatalf("the Live TV provider lost its features:\n%s", out)
			}
			if bytes.Contains(out, []byte("owner@example.com")) || bytes.Contains(out, []byte("Owner Library")) {
				t.Fatalf("the owner's container or library leaked:\n%s", out)
			}
			v := viewProviders(t, out, tc.format)
			if v.attrs["size"] != "3" {
				t.Fatalf("size=%q, want 3 (one appended, one replaced)", v.attrs["size"])
			}
		})
	}

	t.Run("no Live TV keeps the caller's bytes", func(t *testing.T) {
		elevated := `<MediaContainer size="1"><MediaProvider identifier="com.plexapp.plugins.library"/></MediaContainer>`
		out, providers, live, err := graftLiveProviders([]byte(ownXML), []byte(elevated), false)
		if err != nil || string(out) != ownXML || providers != 2 || live != 0 {
			t.Fatalf("out=%s providers=%d live=%d err=%v", out, providers, live, err)
		}
	})
	t.Run("an answer without a container is an error", func(t *testing.T) {
		if _, _, _, err := graftLiveProviders([]byte(`<Error/>`), []byte(elevatedXML), false); err == nil {
			t.Fatal("want an error for an own answer without MediaContainer")
		}
		if _, _, _, err := graftLiveProviders([]byte(ownJSON), []byte(`{"MediaContainer":{"MediaProvider":{}}}`), true); err == nil {
			t.Fatal("want an error for a MediaProvider that isn't a list")
		}
	})
}
