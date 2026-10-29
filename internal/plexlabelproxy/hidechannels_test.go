package plexlabelproxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
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

const hideProvider = "/tv.plex.providers.epg.xmltv:4"

// hiddenIdent is the hidden channel's guide identifier in the fixtures:
// Tunarr's id for channel 99.
const hiddenIdent = "C99.114.tunarr.com"

// hideFixturePaths maps PMS paths to the fixture served for them,
// testdata/hidechannels/<name>.json or .xml. The fixtures have channels 1, 2
// and 99, a program airing only on 99 ("Only On Test") and one airing on both
// 2 and 99 ("On Science And Test").
var hideFixturePaths = map[string]string{
	hideProvider + "/lineups/dvr/channels": "lineups_channels",
	hideProvider + "/grid":                 "grid",
	hideProvider + "/hubs/discover":        "hubs_discover",
	hideProvider + "/library/search":       "library_search",
	hideProvider + "/sections/1/all":       "sections_all",
	"/livetv/dvrs":                         "livetv_dvrs",
	"/media/grabbers/devices/1/channels":   "device_channels",
	"/media/grabbers/devices/2/channels":   "device_channels",
}

// hideFixture is a PMS answering with the fixtures (JSON when asked for, XML
// otherwise, gzipped when gzip is set) and a plex.tv whose share list has one
// shared user, friend-token. owner-device-token is one of the owner's apps.
type hideFixture struct {
	pms, plexTV      *httptest.Server
	gzip             atomic.Bool
	plexTVDown       atomic.Bool
	friendUnshared   atomic.Bool
	dvrsDown         atomic.Bool
	shareListFetches atomic.Int32
	logs             syncBuffer

	mu    sync.Mutex
	calls []pmsCall
	// dvrsXML, when set, replaces the /livetv/dvrs fixture.
	dvrsXML string
}

type pmsCall struct {
	path, viewAs string
	conditional  bool
}

func newHideFixture(t *testing.T) *hideFixture {
	t.Helper()
	f := &hideFixture{}
	f.pms = httptest.NewServer(http.HandlerFunc(f.servePMS))
	t.Cleanup(f.pms.Close)
	f.plexTV = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/servers/machine-1/shared_servers" || r.Header.Get("X-Plex-Token") != "owner-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if f.plexTVDown.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		f.shareListFetches.Add(1)
		shared := `<SharedServer id="1" username="friend" userID="42" accessToken="friend-token"/>`
		if f.friendUnshared.Load() {
			shared = `<SharedServer id="2" username="other" userID="43" accessToken="other-token"/>`
		}
		_, _ = w.Write([]byte("<MediaContainer>" + shared + "</MediaContainer>"))
	}))
	t.Cleanup(f.plexTV.Close)
	// Whatever a test did, the logs never carry a raw token.
	t.Cleanup(func() {
		logs := f.logs.String()
		for _, token := range []string{"friend-token", "owner-token", "owner-device-token", "other-token", "label-token"} {
			if strings.Contains(logs, token) {
				t.Errorf("logs carry the raw token %q:\n%s", token, logs)
			}
		}
	})
	return f
}

func (f *hideFixture) servePMS(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, pmsCall{
		path:        r.URL.Path,
		viewAs:      r.Header.Get(ViewAsHeader),
		conditional: r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "",
	})
	dvrsXML := f.dvrsXML
	f.mu.Unlock()
	if r.URL.Path == "/identity" {
		w.Header().Set("Content-Type", "application/xml")
		if r.Header.Get("X-Plex-Token") != "owner-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`<MediaContainer machineIdentifier="machine-1"/>`))
		return
	}
	if r.URL.Path == "/livetv/dvrs" && f.dvrsDown.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if r.Header.Get("If-None-Match") == `"etag-1"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	asJSON := strings.Contains(r.Header.Get("Accept"), "json")
	var body []byte
	switch name, ok := hideFixturePaths[r.URL.Path]; {
	case r.URL.Path == "/livetv/dvrs" && dvrsXML != "":
		body, asJSON = []byte(dvrsXML), false
	case r.URL.Path == "/livetv/empty":
		w.Header().Set("Content-Type", "application/json")
		return
	case r.URL.Path == "/livetv/odd":
		// A shape the filter doesn't know that still names the channel.
		body, asJSON = []byte(`{"MediaContainer":{"size":1,"Odd":[{"guideChannel":"`+hiddenIdent+`"}]}}`), true
	case ok:
		if name == "grid" && r.URL.Query().Get("clean") == "1" {
			name = "grid_clean"
		}
		ext := ".xml"
		if asJSON {
			ext = ".json"
		}
		b, err := os.ReadFile(filepath.Join("testdata", "hidechannels", name+ext))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		body = b
		if name == "sections_all" {
			w.Header().Set("X-Plex-Container-Start", "0")
			w.Header().Set("X-Plex-Container-Total-Size", "10")
		}
	default:
		body, asJSON = []byte(`<?xml version="1.0" encoding="UTF-8"?>`+"\n"+`<MediaContainer size="0"/>`), false
	}
	if asJSON {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "text/xml;charset=utf-8")
	}
	w.Header().Set("X-Plex-Content-Original-Length", strconv.Itoa(len(body)))
	if f.gzip.Load() && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		_, _ = gz.Write(body)
		_ = gz.Close()
		body = buf.Bytes()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("X-Plex-Content-Compressed-Length", strconv.Itoa(len(body)))
	}
	_, _ = w.Write(body)
}

// proxy is a proxy hiding channel 99 in front of the fixture; edit changes
// its config.
func (f *hideFixture) proxy(t *testing.T, edit func(*Config)) *Proxy {
	t.Helper()
	cfg := Config{
		Upstream:         f.pms.URL,
		Token:            "label-token",
		OwnerToken:       "owner-token",
		ElevateLiveTV:    true,
		TokenAuthorizer:  staticTokenAuthorizer{"friend-token": true, "owner-device-token": true, "other-token": true},
		Logger:           log.New(&f.logs, "", 0),
		PlexTVURL:        f.plexTV.URL,
		HideLiveChannels: []string{"99"},
	}
	if edit != nil {
		edit(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	if p.viewers != nil {
		// A share list that needs confirming is refetched at once, not a
		// minute later.
		p.viewers.retryAfter = 0
	}
	return p
}

// settleOwnerDevice makes token one of the owner's devices: first seen, then
// a share list (without it) fetched and confirmed after that.
func settleOwnerDevice(t *testing.T, p *Proxy, token string) {
	t.Helper()
	fp := tokenFingerprint(token)
	p.viewers.classify(fp, time.Now())
	for i := 0; i < 2; i++ {
		if err := p.viewers.refreshAccounts(context.Background(), time.Now()); err != nil {
			t.Fatalf("share list: %v", err)
		}
	}
	if c, _ := p.viewers.classify(fp, time.Now()); c != viewerOwnerDevice {
		t.Fatalf("%s is %s, want owner_device", token, c)
	}
}

// baseline is the same proxy without -hide-live-channel.
func (f *hideFixture) baseline(t *testing.T) *Proxy {
	t.Helper()
	return f.proxy(t, func(c *Config) { c.HideLiveChannels = nil })
}

func (f *hideFixture) callsTo(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.path == path {
			n++
		}
	}
	return n
}

func (f *hideFixture) viewAsForwarded() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.viewAs != "" {
			return true
		}
	}
	return false
}

// hideServe sends method path to p as the holder of token (none when ""),
// asking for format "json" or "xml"; header lists extra name, value pairs.
func hideServe(p *Proxy, method, path, token, format string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "127.0.0.1:12345"
	if token != "" {
		req.Header.Set("X-Plex-Token", token)
	}
	if format == "json" {
		req.Header.Set("Accept", "application/json")
	} else {
		req.Header.Set("Accept", "application/xml")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// hideBody is rec's body, gunzipped when it was sent gzipped, after checking
// Content-Length against what was sent.
func hideBody(t *testing.T, rec *httptest.ResponseRecorder) []byte {
	t.Helper()
	raw := rec.Body.Bytes()
	if cl := rec.Header().Get("Content-Length"); cl != "" && cl != strconv.Itoa(len(raw)) {
		t.Fatalf("Content-Length %s for a %d-byte body", cl, len(raw))
	}
	if rec.Header().Get("Content-Encoding") != "gzip" {
		return raw
	}
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	out, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return out
}

// containerSummary is a MediaContainer's size, totalSize, direct entries by
// name and its hubs' sizes.
type containerSummary struct {
	size, totalSize string
	entries         map[string]int
	hubSizes        []string
}

func summarizeContainer(t *testing.T, body []byte, format string) containerSummary {
	t.Helper()
	s := containerSummary{entries: map[string]int{}}
	if format == "json" {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		var doc struct{ MediaContainer map[string]any }
		if err := dec.Decode(&doc); err != nil {
			t.Fatalf("decode JSON: %v\n%s", err, body)
		}
		mc := doc.MediaContainer
		s.size, s.totalSize = jsonString(mc["size"]), jsonString(mc["totalSize"])
		for k, v := range mc {
			switch v := v.(type) {
			case []any:
				s.entries[k] = len(v)
				for _, el := range v {
					if hub, ok := el.(map[string]any); ok && k == "Hub" {
						s.hubSizes = append(s.hubSizes, jsonString(hub["size"]))
					}
				}
			case map[string]any:
				s.entries[k] = 1
			}
		}
		return s
	}
	dec := xml.NewDecoder(bytes.NewReader(body))
	depth := 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode XML: %v\n%s", err, body)
		}
		switch e := tok.(type) {
		case xml.StartElement:
			depth++
			switch depth {
			case 1:
				s.size, s.totalSize = attrValue(e.Attr, "size"), attrValue(e.Attr, "totalSize")
			case 2:
				s.entries[e.Name.Local]++
				if e.Name.Local == "Hub" {
					s.hubSizes = append(s.hubSizes, attrValue(e.Attr, "size"))
				}
			}
		case xml.EndElement:
			depth--
		}
	}
	return s
}

func TestHideLiveChannel_SharedUserNeverSeesTheChannel(t *testing.T) {
	f := newHideFixture(t)
	p := f.proxy(t, nil)
	cases := []struct {
		name, path                       string
		size, totalSize, totalSizeHeader string
		// entries left in the root container; xmlEntries when XML names them
		// differently.
		entries, xmlEntries map[string]int
		hubSizes            []string
		kept                []string
	}{
		{name: "lineup", path: hideProvider + "/lineups/dvr/channels", size: "2",
			entries: map[string]int{"Channel": 2}, kept: []string{"C1.49.tunarr.com", "C2.50.tunarr.com"}},
		{name: "grid", path: hideProvider + "/grid?type=1,4", size: "3",
			entries: map[string]int{"Metadata": 3}, xmlEntries: map[string]int{"Video": 3},
			kept: []string{"C1.49.tunarr.com", "C2.50.tunarr.com", "On Science And Test", "1700000000"}},
		{name: "hubs", path: hideProvider + "/hubs/discover?includeRecentChannels=1", size: "2",
			entries: map[string]int{"Hub": 2}, hubSizes: []string{"1", "1"},
			kept: []string{"On Cartoons", "Recent Channels", "C2.50.tunarr.com", "On Science"}},
		{name: "search", path: hideProvider + "/library/search?query=on", size: "1",
			entries: map[string]int{"SearchResult": 1}, kept: []string{"On Science", "0.5"}},
		{name: "dvrs", path: "/livetv/dvrs", size: "1",
			entries: map[string]int{"Dvr": 1}, kept: []string{"C1.49.tunarr.com", "C2.50.tunarr.com"}},
		{name: "tuner channels", path: "/media/grabbers/devices/1/channels", size: "2",
			entries: map[string]int{"DeviceChannel": 2}},
		{name: "paged section", path: hideProvider + "/sections/1/all?X-Plex-Container-Start=0&X-Plex-Container-Size=3",
			size: "2", totalSize: "9", totalSizeHeader: "9",
			entries: map[string]int{"Metadata": 2}, xmlEntries: map[string]int{"Video": 2}},
	}
	for _, c := range cases {
		for _, format := range []string{"json", "xml"} {
			for _, gz := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/gzip=%v", c.name, format, gz), func(t *testing.T) {
					f.gzip.Store(gz)
					rec := hideServe(p, http.MethodGet, c.path, "friend-token", format)
					if rec.Code != http.StatusOK {
						t.Fatalf("status %d", rec.Code)
					}
					body := hideBody(t, rec)
					if format == "xml" && !bytes.HasPrefix(body, []byte(`<?xml version="1.0" encoding="UTF-8"?>`+"\n<MediaContainer")) {
						t.Errorf("XML declaration or spacing changed:\n%s", body)
					}
					for _, gone := range []string{hiddenIdent, "Only On Test", "99 99 Test", "ch99.png"} {
						if bytes.Contains(body, []byte(gone)) {
							t.Errorf("%q is still in:\n%s", gone, body)
						}
					}
					for _, kept := range c.kept {
						if !bytes.Contains(body, []byte(kept)) {
							t.Errorf("%q was removed:\n%s", kept, body)
						}
					}
					s := summarizeContainer(t, body, format)
					if s.size != c.size || s.totalSize != c.totalSize {
						t.Errorf("size=%q totalSize=%q, want %q %q", s.size, s.totalSize, c.size, c.totalSize)
					}
					want := c.entries
					if format == "xml" && c.xmlEntries != nil {
						want = c.xmlEntries
					}
					for name, n := range want {
						if s.entries[name] != n {
							t.Errorf("%d %s entries, want %d", s.entries[name], name, n)
						}
					}
					if !slices.Equal(s.hubSizes, c.hubSizes) {
						t.Errorf("hub sizes %q, want %q", s.hubSizes, c.hubSizes)
					}
					h := rec.Header()
					if got := h.Get("X-Plex-Container-Total-Size"); got != c.totalSizeHeader {
						t.Errorf("X-Plex-Container-Total-Size %q, want %q", got, c.totalSizeHeader)
					}
					if got := h.Get("Cache-Control"); got != "private, no-store" {
						t.Errorf("Cache-Control %q", got)
					}
					if h.Get("X-Plex-Content-Original-Length") != "" || h.Get("X-Plex-Content-Compressed-Length") != "" {
						t.Errorf("stale X-Plex-Content-*-Length headers kept: %v", h)
					}
					if gotGzip := h.Get("Content-Encoding") == "gzip"; gotGzip != gz {
						t.Errorf("gzipped=%v, want %v", gotGzip, gz)
					}
				})
			}
		}
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "plexlabelproxy_hide: outcome=removed path="+hideProvider+"/grid viewer=friend removed=") {
		t.Errorf("no removed line for the grid:\n%s", logs)
	}
	if strings.Contains(logs, "outcome=leak_suspect") || strings.Contains(logs, "outcome=unparsed") {
		t.Errorf("a known shape was flagged:\n%s", logs)
	}
}

// gridItem is a guide program: its airings' channels and Channel tags.
type gridItem struct {
	title          string
	airingChannels []string
	channelTags    []string
}

func decodeGrid(t *testing.T, body []byte, format string) []gridItem {
	t.Helper()
	type media struct {
		JSONIdent string `json:"channelIdentifier"`
		XMLIdent  string `xml:"channelIdentifier,attr"`
	}
	type tag struct {
		JSONTag string `json:"tag"`
		XMLTag  string `xml:"tag,attr"`
	}
	type item struct {
		JSONTitle string  `json:"title"`
		XMLTitle  string  `xml:"title,attr"`
		Media     []media `json:"Media" xml:"Media"`
		Channel   []tag   `json:"Channel" xml:"Channel"`
	}
	var items []item
	if format == "json" {
		var doc struct {
			MediaContainer struct {
				Metadata []item
			}
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatalf("decode JSON grid: %v", err)
		}
		items = doc.MediaContainer.Metadata
	} else {
		var doc struct {
			Video []item `xml:"Video"`
		}
		if err := xml.Unmarshal(body, &doc); err != nil {
			t.Fatalf("decode XML grid: %v", err)
		}
		items = doc.Video
	}
	var out []gridItem
	for _, it := range items {
		g := gridItem{title: it.JSONTitle + it.XMLTitle}
		for _, m := range it.Media {
			g.airingChannels = append(g.airingChannels, m.JSONIdent+m.XMLIdent)
		}
		for _, c := range it.Channel {
			g.channelTags = append(g.channelTags, c.JSONTag+c.XMLTag)
		}
		out = append(out, g)
	}
	return out
}

func TestHideLiveChannel_KeepsAProgramsOtherAirings(t *testing.T) {
	f := newHideFixture(t)
	p := f.proxy(t, nil)
	for _, format := range []string{"json", "xml"} {
		items := decodeGrid(t, hideBody(t, hideServe(p, http.MethodGet, hideProvider+"/grid", "friend-token", format)), format)
		var titles []string
		for _, it := range items {
			titles = append(titles, it.title)
			if it.title != "On Science And Test" {
				continue
			}
			if !slices.Equal(it.airingChannels, []string{"C2.50.tunarr.com"}) || !slices.Equal(it.channelTags, []string{"2 2 Science"}) {
				t.Errorf("%s: the program on 2 and 99 kept airings %q and tags %q, want channel 2's only", format, it.airingChannels, it.channelTags)
			}
		}
		if !slices.Equal(titles, []string{"On Science", "On Science And Test", "On Cartoons"}) {
			t.Errorf("%s: programs %q", format, titles)
		}
	}
}

// recentChannels is the size of hubs/discover's Recent Channels hub and the
// numbers of the channels on it.
func recentChannels(t *testing.T, body []byte, format string) (string, []string) {
	t.Helper()
	type channel struct {
		JSONIdentifier string `json:"identifier"`
		XMLIdentifier  string `xml:"identifier,attr"`
	}
	type hub struct {
		JSONHubIdentifier string      `json:"hubIdentifier"`
		XMLHubIdentifier  string      `xml:"hubIdentifier,attr"`
		JSONSize          json.Number `json:"size"`
		XMLSize           string      `xml:"size,attr"`
		Directory         []channel   `json:"Directory" xml:"Directory"`
	}
	var hubs []hub
	if format == "json" {
		var doc struct {
			MediaContainer struct {
				Hub []hub
			}
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatalf("decode JSON hubs: %v", err)
		}
		hubs = doc.MediaContainer.Hub
	} else {
		var doc struct {
			Hub []hub `xml:"Hub"`
		}
		if err := xml.Unmarshal(body, &doc); err != nil {
			t.Fatalf("decode XML hubs: %v", err)
		}
		hubs = doc.Hub
	}
	for _, h := range hubs {
		if h.JSONHubIdentifier+h.XMLHubIdentifier != "recent.channels" {
			continue
		}
		var numbers []string
		for _, c := range h.Directory {
			numbers = append(numbers, c.JSONIdentifier+c.XMLIdentifier)
		}
		return h.JSONSize.String() + h.XMLSize, numbers
	}
	t.Fatalf("no Recent Channels hub:\n%s", body)
	return "", nil
}

func TestHideLiveChannel_HidesItFromTheRecentChannelsShelf(t *testing.T) {
	f := newHideFixture(t)
	p := f.proxy(t, nil)
	settleOwnerDevice(t, p, "owner-device-token")
	discover := hideProvider + "/hubs/discover?includeRecentChannels=1"
	for _, format := range []string{"json", "xml"} {
		size, numbers := recentChannels(t, hideBody(t, hideServe(p, http.MethodGet, discover, "friend-token", format)), format)
		if size != "1" || !slices.Equal(numbers, []string{"2"}) {
			t.Errorf("%s: a shared user's Recent Channels has size %s and channels %q, want 1 and channel 2", format, size, numbers)
		}
		for _, token := range []string{"owner-token", "owner-device-token"} {
			size, numbers := recentChannels(t, hideBody(t, hideServe(p, http.MethodGet, discover, token, format)), format)
			if size != "2" || !slices.Equal(numbers, []string{"2", "99"}) {
				t.Errorf("%s: %s's Recent Channels has size %s and channels %q, want 2 and channels 2 and 99", format, token, size, numbers)
			}
		}
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "plexlabelproxy_hide: outcome=removed path="+hideProvider+"/hubs/discover viewer=friend removed=") {
		t.Errorf("no removed line for the shared user's hubs:\n%s", logs)
	}
	if strings.Contains(logs, "outcome=leak_suspect") {
		t.Errorf("the hubs were flagged as leaking the channel:\n%s", logs)
	}
}

func TestHideLiveChannel_OwnerAndOwnerDevicesSeeEverything(t *testing.T) {
	f := newHideFixture(t)
	p, base := f.proxy(t, nil), f.baseline(t)
	settleOwnerDevice(t, p, "owner-device-token")
	for path := range hideFixturePaths {
		for _, format := range []string{"json", "xml"} {
			for _, token := range []string{"owner-token", "owner-device-token"} {
				want := hideBody(t, hideServe(base, http.MethodGet, path, token, format))
				got := hideBody(t, hideServe(p, http.MethodGet, path, token, format))
				if !bytes.Equal(got, want) {
					t.Errorf("%s %s for %s changed:\n%s\nwant:\n%s", path, format, token, got, want)
				}
			}
		}
	}
	if f.shareListFetches.Load() == 0 {
		t.Fatal("the owner's device was never checked against the share list")
	}
	if strings.Contains(f.logs.String(), "plexlabelproxy_hide: outcome=removed") {
		t.Errorf("the owner's responses were filtered:\n%s", f.logs.String())
	}
}

func TestHideLiveChannel_HidesItFromAnUnsettledCaller(t *testing.T) {
	f := newHideFixture(t)
	p := f.proxy(t, nil)
	gridShows := func(token string) bool {
		t.Helper()
		return bytes.Contains(hideBody(t, hideServe(p, http.MethodGet, hideProvider+"/grid", token, "json")), []byte(hiddenIdent))
	}
	if gridShows("") {
		t.Error("a request without a token saw the hidden channel")
	}
	// The owner's device, first seen while plex.tv is down: no share list
	// settles it, so it's treated like a shared user.
	f.plexTVDown.Store(true)
	if gridShows("owner-device-token") {
		t.Error("an unsettled token saw the hidden channel")
	}
	if !strings.Contains(f.logs.String(), "plexlabelproxy_hide: share list fetch failed") {
		t.Errorf("the failed fetch wasn't logged:\n%s", f.logs.String())
	}
	// plex.tv is back: the first list is confirmed by the next fetch, which
	// settles it as the owner's device.
	f.plexTVDown.Store(false)
	if gridShows("owner-device-token") {
		t.Error("an unconfirmed first share list settled the owner device")
	}
	if !gridShows("owner-device-token") {
		t.Error("the settled owner device still doesn't see the channel")
	}
	// Settled stays settled when a later fetch fails.
	f.plexTVDown.Store(true)
	if err := p.viewers.refreshAccounts(context.Background(), time.Now().Add(accountsMaxAge+time.Minute)); err == nil {
		t.Fatal("the share-list fetch didn't fail")
	}
	if !gridShows("owner-device-token") {
		t.Error("a failed share-list fetch hid the channel from the owner's device")
	}
}

func TestHideLiveChannel_ASharedUserStaysHiddenAfterLeavingTheShareList(t *testing.T) {
	f := newHideFixture(t)
	p := f.proxy(t, nil)
	gridShows := func() bool {
		t.Helper()
		return bytes.Contains(hideBody(t, hideServe(p, http.MethodGet, hideProvider+"/grid", "friend-token", "json")), []byte(hiddenIdent))
	}
	if gridShows() {
		t.Fatal("the shared user saw the hidden channel")
	}
	// plex.tv's next list lacks them (a glitch, or unshared): they're not
	// taken for one of the owner's devices.
	f.friendUnshared.Store(true)
	if err := p.viewers.refreshAccounts(context.Background(), time.Now().Add(accountsMaxAge+time.Minute)); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if n := f.shareListFetches.Load(); n != 2 {
		t.Fatalf("%d share-list fetches, want 2", n)
	}
	if gridShows() {
		t.Error("a shared user missing from a later share list saw the hidden channel")
	}
}

func TestLiveViewerTracker_ClassifyDistrustsASuddenlyEmptyShareList(t *testing.T) {
	accounts := map[string]plexAccount{"fp-friend": {id: "42", username: "friend"}}
	fail := false
	tr := newLiveViewerTracker(func(context.Context) (shareList, error) {
		if fail {
			return shareList{}, errors.New("plex.tv down")
		}
		out := map[string]plexAccount{}
		for k, v := range accounts {
			out[k] = v
		}
		return shareList{accounts: out, machineIdentifier: "m"}, nil
	})
	tr.retryAfter = time.Minute
	ctx := context.Background()
	t0 := time.Unix(1_800_000_000, 0)
	want := func(fp string, at time.Time, wantClass viewerClass, wantSettled bool) {
		t.Helper()
		if c, settled := tr.classify(fp, at); c != wantClass || settled != wantSettled {
			t.Fatalf("%s at +%s: %s settled=%v, want %s settled=%v", fp, at.Sub(t0), c, settled, wantClass, wantSettled)
		}
	}

	want("fp-friend", t0, viewerUnsettled, false)
	_ = tr.refreshAccounts(ctx, t0)
	want("fp-friend", t0, viewerFriend, true)
	want("fp-device", t0.Add(time.Minute), viewerUnsettled, false)

	// plex.tv suddenly lists nobody: not believed yet, so the new token
	// stays unsettled (hidden), and the friend stays a friend.
	accounts = map[string]plexAccount{}
	_ = tr.refreshAccounts(ctx, t0.Add(2*time.Minute))
	want("fp-device", t0.Add(2*time.Minute), viewerUnsettled, false)
	want("fp-friend", t0.Add(2*time.Minute), viewerFriend, true)

	// It's asked again after the retry delay, not at the regular refresh,
	// and a second empty list is believed.
	_ = tr.refreshAccounts(ctx, t0.Add(2*time.Minute+30*time.Second))
	want("fp-device", t0.Add(2*time.Minute+30*time.Second), viewerUnsettled, false)
	_ = tr.refreshAccounts(ctx, t0.Add(3*time.Minute))
	want("fp-device", t0.Add(3*time.Minute), viewerOwnerDevice, true)
	want("fp-friend", t0.Add(3*time.Minute), viewerFriend, true)

	// A failed fetch unsettles nobody.
	fail = true
	if err := tr.refreshAccounts(ctx, t0.Add(20*time.Minute)); err == nil {
		t.Fatal("the fetch didn't fail")
	}
	want("fp-device", t0.Add(20*time.Minute), viewerOwnerDevice, true)
	// A token first seen after the last good list isn't settled by it.
	want("fp-new", t0.Add(21*time.Minute), viewerUnsettled, false)
}

func TestLiveViewerTracker_ClassifyDistrustsAFirstEmptyShareList(t *testing.T) {
	tr := newLiveViewerTracker(func(context.Context) (shareList, error) {
		return shareList{accounts: map[string]plexAccount{}, machineIdentifier: "m"}, nil
	})
	ctx := context.Background()
	t0 := time.Unix(1_800_000_000, 0)
	want := func(fp string, at time.Duration, wantClass viewerClass) {
		t.Helper()
		if c, _ := tr.classify(fp, t0.Add(at)); c != wantClass {
			t.Fatalf("%s at +%s: %s, want %s", fp, at, c, wantClass)
		}
	}
	// The proxy starts while plex.tv answers with nobody: a shared user's
	// token, absent from it, isn't taken for the owner's.
	want("fp-shared", 0, viewerUnsettled)
	_ = tr.refreshAccounts(ctx, t0.Add(time.Second))
	want("fp-shared", 2*time.Second, viewerUnsettled)
	// It's asked again after the retry delay; a second empty list is
	// believed, and later empty lists are too.
	_ = tr.refreshAccounts(ctx, t0.Add(accountsRetry+time.Second))
	want("fp-shared", accountsRetry+2*time.Second, viewerOwnerDevice)
	want("fp-later", 3*accountsRetry, viewerUnsettled)
	_ = tr.refreshAccounts(ctx, t0.Add(3*accountsRetry+time.Second))
	want("fp-later", 3*accountsRetry+2*time.Second, viewerOwnerDevice)
}

func TestLiveViewerTracker_ClassifyConfirmsAPartialShareList(t *testing.T) {
	list := []string{"fp-a"}
	tr := newLiveViewerTracker(func(context.Context) (shareList, error) {
		out := map[string]plexAccount{}
		for _, fp := range list {
			out[fp] = plexAccount{id: fp}
		}
		return shareList{accounts: out, machineIdentifier: "m"}, nil
	})
	ctx := context.Background()
	t0 := time.Unix(1_800_000_000, 0)
	step := 0
	fetch := func(accounts ...string) {
		t.Helper()
		list = accounts
		step++
		_ = tr.refreshAccounts(ctx, t0.Add(time.Duration(step)*accountsRetry))
	}
	want := func(fp string, wantClass viewerClass) {
		t.Helper()
		if c, _ := tr.classify(fp, t0.Add(time.Duration(step)*accountsRetry+time.Second)); c != wantClass {
			t.Fatalf("after fetch %d, %s is %s, want %s", step, fp, c, wantClass)
		}
	}

	// The first list may be partial (fp-b is shared too): it settles nobody
	// until the next fetch returns the same list.
	want("fp-b", viewerUnsettled)
	fetch("fp-a")
	want("fp-b", viewerUnsettled)
	fetch("fp-a", "fp-b")
	want("fp-b", viewerFriend)
	want("fp-device", viewerUnsettled)
	fetch("fp-a", "fp-b")
	want("fp-device", viewerOwnerDevice)

	// A list lacking someone the trusted one had (plex.tv answering partly)
	// settles nobody new; one that has everyone again does.
	want("fp-new", viewerUnsettled)
	fetch("fp-a")
	want("fp-new", viewerUnsettled)
	want("fp-b", viewerFriend)
	fetch("fp-a", "fp-b")
	want("fp-new", viewerOwnerDevice)

	// A shrunken list that the next fetch repeats is believed (fp-b was
	// unshared), though fp-b stays a friend.
	want("fp-later", viewerUnsettled)
	fetch("fp-a")
	want("fp-later", viewerUnsettled)
	fetch("fp-a")
	want("fp-later", viewerOwnerDevice)
	want("fp-b", viewerFriend)
}

func TestLiveViewerTracker_ClassifyBoundsWhatItRemembers(t *testing.T) {
	tr := newLiveViewerTracker(func(context.Context) (shareList, error) { return shareList{}, nil })
	t0 := time.Unix(1_800_000_000, 0)
	for i := 0; i < maxTrackedTokens+50; i++ {
		tr.classify(fmt.Sprintf("fp-%d", i), t0.Add(time.Duration(i)*time.Millisecond))
	}
	tr.mu.Lock()
	n := len(tr.seen)
	tr.mu.Unlock()
	if n > maxTrackedTokens {
		t.Fatalf("remembers %d tokens, want at most %d", n, maxTrackedTokens)
	}
	tr.classify("fp-late", t0.Add(trackedTokenTTL+time.Hour))
	tr.mu.Lock()
	n = len(tr.seen)
	tr.mu.Unlock()
	if n != 1 {
		t.Fatalf("remembers %d tokens after a day, want 1", n)
	}
}

func TestLiveViewerTracker_ClassifyKeepsAnActiveOwnerDevice(t *testing.T) {
	tr := newLiveViewerTracker(func(context.Context) (shareList, error) {
		return shareList{accounts: map[string]plexAccount{"fp-friend": {id: "42"}}, machineIdentifier: "m"}, nil
	})
	t0 := time.Unix(1_800_000_000, 0)
	tr.classify("fp-device", t0)
	_ = tr.refreshAccounts(context.Background(), t0.Add(time.Second))
	_ = tr.refreshAccounts(context.Background(), t0.Add(accountsRetry+time.Second))
	for _, at := range []time.Duration{time.Minute, 23 * time.Hour, 46 * time.Hour} {
		// A new token prunes the table; the device, used within the last
		// 24 hours, stays remembered as the owner's.
		tr.classify(fmt.Sprintf("fp-new-%s", at), t0.Add(at))
		if c, settled := tr.classify("fp-device", t0.Add(at)); c != viewerOwnerDevice || !settled {
			t.Fatalf("at +%s the owner's device is %s settled=%v", at, c, settled)
		}
	}
}

func TestHideLiveChannel_HidesTheTunerChannelOfAGuideIdentifier(t *testing.T) {
	deviceChannels := func(t *testing.T, p *Proxy, device string) containerSummary {
		t.Helper()
		return summarizeContainer(t, hideBody(t, hideServe(p, http.MethodGet, "/media/grabbers/devices/"+device+"/channels", "friend-token", "json")), "json")
	}
	t.Run("tunarr identifier", func(t *testing.T) {
		f := newHideFixture(t)
		p := f.proxy(t, func(c *Config) { c.HideLiveChannels = []string{hiddenIdent} })
		if s := deviceChannels(t, p, "1"); s.entries["DeviceChannel"] != 2 || s.size != "2" {
			t.Errorf("tuner channels %+v, want 99's removed", s)
		}
	})
	t.Run("once the channel map pairs it", func(t *testing.T) {
		f := newHideFixture(t)
		f.mu.Lock()
		f.dvrsXML = `<MediaContainer size="1"><Dvr key="4"><Device key="1">
<ChannelMapping channelKey="example-99" deviceIdentifier="99" enabled="1" lineupIdentifier="99"/>
</Device></Dvr></MediaContainer>`
		f.mu.Unlock()
		f.dvrsDown.Store(true)
		p := f.proxy(t, func(c *Config) { c.HideLiveChannels = []string{"example-99"} })
		if desc := p.HiddenLiveChannels(context.Background()); !strings.Contains(desc, "not in Plex's channel map yet") {
			t.Errorf("startup description %q", desc)
		}
		// PMS answers again a minute later.
		f.dvrsDown.Store(false)
		p.hidden.mu.Lock()
		p.hidden.mapTriedAt = time.Now().Add(-2 * hiddenMapMaxAge)
		p.hidden.mu.Unlock()
		if s := deviceChannels(t, p, "1"); s.entries["DeviceChannel"] != 2 || s.size != "2" {
			t.Errorf("tuner channels %+v, want 99's removed", s)
		}
		// The map isn't fetched for every response...
		calls := f.callsTo("/livetv/dvrs")
		deviceChannels(t, p, "1")
		if n := f.callsTo("/livetv/dvrs"); n != calls {
			t.Errorf("channel map fetched again (%d -> %d) within a minute", calls, n)
		}
		// ...but a minute later it is, and follows Plex moving the channel to
		// another tuner channel.
		f.mu.Lock()
		f.dvrsXML = strings.ReplaceAll(f.dvrsXML, `Identifier="99"`, `Identifier="2"`)
		f.mu.Unlock()
		p.hidden.mu.Lock()
		p.hidden.mapTriedAt = time.Now().Add(-2 * hiddenMapMaxAge)
		p.hidden.mu.Unlock()
		var doc struct {
			MediaContainer struct {
				DeviceChannel []struct {
					Identifier string `json:"identifier"`
				}
			}
		}
		body := hideBody(t, hideServe(p, http.MethodGet, "/media/grabbers/devices/1/channels", "friend-token", "json"))
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, c := range doc.MediaContainer.DeviceChannel {
			ids = append(ids, c.Identifier)
		}
		if !slices.Equal(ids, []string{"1", "99"}) {
			t.Errorf("after the remap, tuner channels %q, want 1 and 99", ids)
		}
	})
	t.Run("only on its own device", func(t *testing.T) {
		// Two DVRs whose tuners both have a channel 99; only the first's is
		// hidden.
		f := newHideFixture(t)
		f.mu.Lock()
		f.dvrsXML = `<MediaContainer size="2">
<Dvr key="4"><Device key="1"><ChannelMapping channelKey="example-99" deviceIdentifier="99" enabled="1" lineupIdentifier="99"/></Device></Dvr>
<Dvr key="5"><Device key="2"><ChannelMapping channelKey="other-99" deviceIdentifier="99" enabled="1" lineupIdentifier="99"/></Device></Dvr>
</MediaContainer>`
		f.mu.Unlock()
		p := f.proxy(t, func(c *Config) { c.HideLiveChannels = []string{"example-99"} })
		p.HiddenLiveChannels(context.Background())
		if s := deviceChannels(t, p, "1"); s.entries["DeviceChannel"] != 2 {
			t.Errorf("device 1's channels %+v, want 99's removed", s)
		}
		if s := deviceChannels(t, p, "2"); s.entries["DeviceChannel"] != 3 {
			t.Errorf("device 2's channels %+v, want all kept", s)
		}
		dvrs := hideBody(t, hideServe(p, http.MethodGet, "/livetv/dvrs", "friend-token", "xml"))
		if bytes.Contains(dvrs, []byte("example-99")) || !bytes.Contains(dvrs, []byte("other-99")) {
			t.Errorf("DVR mappings:\n%s", dvrs)
		}
		if rec := hideServe(p, http.MethodPost, "/livetv/dvrs/5/channels/other-99/tune", "friend-token", "xml"); rec.Code != http.StatusOK {
			t.Errorf("tuning the other DVR's 99: status %d", rec.Code)
		}
		if rec := hideServe(p, http.MethodPost, "/livetv/dvrs/4/channels/example-99/tune", "friend-token", "xml"); rec.Code != http.StatusNotFound {
			t.Errorf("tuning the hidden channel: status %d", rec.Code)
		}
	})
}

func TestHideLiveChannel_FollowsANumberPlexMovedToAnotherChannel(t *testing.T) {
	f := newHideFixture(t)
	setMap := func(mappings string) {
		f.mu.Lock()
		f.dvrsXML = `<MediaContainer size="1"><Dvr key="4"><Device key="1">` + mappings + `</Device></Dvr></MediaContainer>`
		f.mu.Unlock()
	}
	setMap(`<ChannelMapping channelKey="example-a" deviceIdentifier="99" enabled="1" lineupIdentifier="99"/>`)
	p := f.proxy(t, nil)
	tune := func(channel string) int {
		return hideServe(p, http.MethodPost, "/livetv/dvrs/4/channels/"+channel+"/tune", "friend-token", "xml").Code
	}
	if code := tune("example-a"); code != http.StatusNotFound {
		t.Fatalf("tuning 99 (example-a): status %d", code)
	}
	// Plex now has 99 on example-b, and example-a is channel 98.
	setMap(`<ChannelMapping channelKey="example-b" deviceIdentifier="99" enabled="1" lineupIdentifier="99"/>
<ChannelMapping channelKey="example-a" deviceIdentifier="98" enabled="1" lineupIdentifier="98"/>`)
	p.hidden.mu.Lock()
	p.hidden.mapTriedAt = time.Now().Add(-2 * hiddenMapMaxAge)
	p.hidden.mu.Unlock()
	// The old identifier is asked about first: the stale map is reread
	// before it's judged.
	if code := tune("example-a"); code != http.StatusOK {
		t.Errorf("tuning 98 (example-a, 99's old identifier): status %d", code)
	}
	if code := tune("example-b"); code != http.StatusNotFound {
		t.Errorf("tuning 99 (example-b): status %d", code)
	}
}

func TestHideLiveChannel_SharedViewIsNeverAnswered304(t *testing.T) {
	f := newHideFixture(t)
	p := f.proxy(t, nil)
	grid := hideProvider + "/grid"
	conditional := []string{"If-None-Match", `"etag-1"`, "If-Modified-Since", "Mon, 01 Jan 2024 00:00:00 GMT"}
	lastConditional := func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.calls[len(f.calls)-1].conditional
	}
	// A client that cached the guide unfiltered gets the filtered guide.
	rec := hideServe(p, http.MethodGet, grid, "friend-token", "json", conditional...)
	if rec.Code != http.StatusOK || lastConditional() {
		t.Fatalf("shared user: status %d, validators forwarded=%v", rec.Code, lastConditional())
	}
	if body := hideBody(t, rec); bytes.Contains(body, []byte(hiddenIdent)) {
		t.Errorf("shared user's guide lists the hidden channel:\n%s", body)
	}
	// So does the owner's preview.
	if rec := hideServe(p, http.MethodGet, grid, "owner-token", "json", append(conditional, ViewAsHeader, "friend")...); rec.Code != http.StatusOK || lastConditional() {
		t.Errorf("owner's preview: status %d, validators forwarded=%v", rec.Code, lastConditional())
	}
	// The owner keeps them, and so does anyone off the Live TV paths.
	if rec := hideServe(p, http.MethodGet, grid, "owner-token", "json", conditional...); rec.Code != http.StatusNotModified || !lastConditional() {
		t.Errorf("owner: status %d, validators forwarded=%v", rec.Code, lastConditional())
	}
	if rec := hideServe(p, http.MethodGet, "/library/sections", "friend-token", "json", conditional...); rec.Code != http.StatusNotModified || !lastConditional() {
		t.Errorf("library: status %d, validators forwarded=%v", rec.Code, lastConditional())
	}
}

func TestHideLiveChannel_RefusesToTuneIt(t *testing.T) {
	f := newHideFixture(t)
	p := f.proxy(t, func(c *Config) {
		c.AbuseBlockThreshold = 2
		c.AbuseBlockWindow = time.Minute
		c.AbuseBlockDuration = time.Hour
	})
	tunePath := func(channel string) string { return "/livetv/dvrs/4/channels/" + channel + "/tune" }
	// A shared user, again and again, by identifier and by number: 404, and
	// PMS never hears of it.
	for i := 0; i < 3; i++ {
		for _, channel := range []string{hiddenIdent, "99"} {
			if rec := hideServe(p, http.MethodPost, tunePath(channel), "friend-token", "xml"); rec.Code != http.StatusNotFound {
				t.Fatalf("shared user tuning %s: status %d, want 404", channel, rec.Code)
			}
		}
	}
	if f.callsTo(tunePath(hiddenIdent)) != 0 || f.callsTo(tunePath("99")) != 0 {
		t.Fatal("a refused tune reached PMS")
	}
	// Not a bad attempt: the same user still tunes other channels.
	if rec := hideServe(p, http.MethodPost, tunePath("C2.50.tunarr.com"), "friend-token", "xml"); rec.Code != http.StatusOK {
		t.Fatalf("shared user tuning channel 2: status %d", rec.Code)
	}
	if f.callsTo(tunePath("C2.50.tunarr.com")) != 1 {
		t.Fatal("channel 2's tune didn't reach PMS")
	}
	// The owner and the owner's device tune it. (The device is first seen
	// after the shared user's fetch, so it waits out the share list's retry
	// delay, about a minute, before a fetch can settle it.)
	p.viewers.mu.Lock()
	p.viewers.retryAfter = 0
	p.viewers.mu.Unlock()
	for i, token := range []string{"owner-token", "owner-device-token"} {
		if rec := hideServe(p, http.MethodPost, tunePath(hiddenIdent), token, "xml"); rec.Code != http.StatusOK {
			t.Fatalf("%s tuning the hidden channel: status %d", token, rec.Code)
		}
		if f.callsTo(tunePath(hiddenIdent)) != i+1 {
			t.Fatalf("%s's tune didn't reach PMS", token)
		}
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "plexlabelproxy_audit: outcome=deny_hidden_channel method=POST path="+tunePath(hiddenIdent)) ||
		!strings.Contains(logs, "viewer=friend") {
		t.Errorf("no deny_hidden_channel audit line:\n%s", logs)
	}
	hideServe(p, http.MethodGet, hideProvider+"/grid", "friend-token", "json")
	p.emitAuditSummary()
	if logs := f.logs.String(); !strings.Contains(logs, "hidden_removed=1 hidden_refused=6 hidden_leak_suspect=0") {
		t.Errorf("summary counters wrong:\n%s", logs)
	}
}

func TestHideLiveChannel_RefusesItNestedInAStreamStart(t *testing.T) {
	f := newHideFixture(t)
	p := f.proxy(t, nil)
	tune99 := "/livetv/dvrs/4/channels/" + hiddenIdent + "/tune"
	server99 := "server://machine-1/com.plexapp.plugins.library/livetv/dvrs/4/channels/99/tune"
	send := func(target, token, contentType, body string) *httptest.ResponseRecorder {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req := httptest.NewRequest(http.MethodPost, target, rd)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Plex-Token", token)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}
	type request struct{ name, target, contentType, body string }
	nested := []request{
		{"transcode path", "/video/:/transcode/universal/start?path=" + url.QueryEscape(tune99), "", ""},
		{"transcode path escaped twice", "/video/:/transcode/universal/start?path=" + url.QueryEscape(url.QueryEscape(tune99)), "", ""},
		{"play queue uri", "/playQueues?type=video&uri=" + url.QueryEscape(server99), "", ""},
		{"play queue form body", "/playQueues?type=video", "application/x-www-form-urlencoded", "uri=" + url.QueryEscape(server99)},
		{"play queue JSON body", "/playQueues?type=video", "application/json", `{"uri":"` + server99 + `"}`},
	}
	for _, rq := range nested {
		if rec := send(rq.target, "friend-token", rq.contentType, rq.body); rec.Code != http.StatusNotFound {
			t.Errorf("%s: a shared user got %d, want 404", rq.name, rec.Code)
		}
	}
	if n := f.callsTo("/video/:/transcode/universal/start") + f.callsTo("/playQueues"); n != 0 {
		t.Fatalf("%d refused stream starts reached PMS", n)
	}
	// Channel 2 nested the same way passes, and so does the owner's 99.
	tune2 := "/video/:/transcode/universal/start?path=" + url.QueryEscape("/livetv/dvrs/4/channels/C2.50.tunarr.com/tune")
	if rec := send(tune2, "friend-token", "", ""); rec.Code != http.StatusOK {
		t.Errorf("channel 2: status %d", rec.Code)
	}
	for _, rq := range nested {
		if rec := send(rq.target, "owner-token", rq.contentType, rq.body); rec.Code != http.StatusOK {
			t.Errorf("%s: the owner got %d", rq.name, rec.Code)
		}
	}
	// The owner's form body still reaches PMS whole.
	if n := f.callsTo("/playQueues"); n != 3 {
		t.Errorf("%d of the owner's play queues reached PMS, want 3", n)
	}
}

func TestHideLiveChannel_ResolvesItThroughTheDVRChannelMap(t *testing.T) {
	f := newHideFixture(t)
	f.mu.Lock()
	f.dvrsXML = `<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer size="1"><Dvr key="4"><Device key="1">
<ChannelMapping channelKey="example-55" deviceIdentifier="55" enabled="1" lineupIdentifier="55"/>
<ChannelMapping channelKey="example-2" deviceIdentifier="2" enabled="1" lineupIdentifier="2"/>
</Device></Dvr></MediaContainer>`
	f.mu.Unlock()
	hide55 := func(c *Config) { c.HideLiveChannels = []string{"55"} }
	tunePath := func(channel string) string { return "/livetv/dvrs/4/channels/" + channel + "/tune" }

	// At startup.
	p := f.proxy(t, hide55)
	if desc := p.HiddenLiveChannels(context.Background()); !strings.Contains(desc, "example-55") || !strings.Contains(desc, "in Plex's channel map") {
		t.Errorf("startup description %q", desc)
	}
	if rec := hideServe(p, http.MethodPost, tunePath("example-55"), "friend-token", "xml"); rec.Code != http.StatusNotFound {
		t.Fatalf("tuning the hidden channel by its mapped id: status %d", rec.Code)
	}
	// An id the map doesn't hide passes, without refetching the map.
	if rec := hideServe(p, http.MethodPost, tunePath("example-2"), "friend-token", "xml"); rec.Code != http.StatusOK {
		t.Fatalf("tuning channel 2: status %d", rec.Code)
	}
	if n := f.callsTo("/livetv/dvrs"); n != 1 {
		t.Errorf("channel map fetched %d times, want 1", n)
	}

	// Lazily, on the first request naming an unknown channel.
	p = f.proxy(t, hide55)
	if rec := hideServe(p, http.MethodPost, tunePath("example-55"), "friend-token", "xml"); rec.Code != http.StatusNotFound {
		t.Fatalf("tuning the hidden channel before the map was read: status %d", rec.Code)
	}
	if f.callsTo(tunePath("example-55")) != 0 {
		t.Fatal("a refused tune reached PMS")
	}
}

func TestHideLiveChannel_PreviewHeaderIsTheOwnersOnlyAndNeverForwarded(t *testing.T) {
	f := newHideFixture(t)
	p := f.proxy(t, nil)
	preview := []string{ViewAsHeader, "friend"}
	grid := hideProvider + "/grid"

	// The owner asking for the shared view gets it.
	if body := hideBody(t, hideServe(p, http.MethodGet, grid, "owner-token", "json", preview...)); bytes.Contains(body, []byte(hiddenIdent)) {
		t.Errorf("the owner's preview still lists the hidden channel:\n%s", body)
	}
	if logs := f.logs.String(); !strings.Contains(logs, "plexlabelproxy_hide: outcome=removed path="+grid+" viewer=preview") {
		t.Errorf("no preview line:\n%s", logs)
	}
	if rec := hideServe(p, http.MethodPost, "/livetv/dvrs/4/channels/"+hiddenIdent+"/tune", "owner-token", "xml", preview...); rec.Code != http.StatusNotFound {
		t.Errorf("the owner's preview tune: status %d, want 404", rec.Code)
	}
	// Without the owner token the header does nothing.
	settleOwnerDevice(t, p, "owner-device-token")
	if body := hideBody(t, hideServe(p, http.MethodGet, grid, "owner-device-token", "json", preview...)); !bytes.Contains(body, []byte(hiddenIdent)) {
		t.Error("the header hid the channel from the owner's device")
	}
	hideServe(p, http.MethodGet, grid, "friend-token", "json", preview...)
	// Hiding off, it's still stripped.
	hideServe(f.baseline(t), http.MethodGet, grid, "owner-token", "json", preview...)
	if f.callsTo(grid) != 4 {
		t.Fatalf("%d grid requests reached PMS, want 4", f.callsTo(grid))
	}
	if f.viewAsForwarded() {
		t.Error("PMS saw the preview header")
	}
}

func TestHideLiveChannel_OffByDefault(t *testing.T) {
	f := newHideFixture(t)
	p := f.baseline(t)
	if p.hidden != nil || p.viewers != nil {
		t.Fatal("hiding state set up without -hide-live-channel")
	}
	if body := hideBody(t, hideServe(p, http.MethodGet, hideProvider+"/grid", "friend-token", "json")); !bytes.Contains(body, []byte(hiddenIdent)) {
		t.Error("channel 99 hidden without -hide-live-channel")
	}
	tune := "/livetv/dvrs/4/channels/" + hiddenIdent + "/tune"
	if rec := hideServe(p, http.MethodPost, tune, "friend-token", "xml"); rec.Code != http.StatusOK || f.callsTo(tune) != 1 {
		t.Errorf("tune refused without -hide-live-channel: status %d", rec.Code)
	}
	if strings.Contains(f.logs.String(), "plexlabelproxy_hide") {
		t.Errorf("hide lines logged:\n%s", f.logs.String())
	}
}

func TestHideLiveChannel_NeedsTheOwnerTokenAndLiveTVElevation(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no owner token":  {Upstream: "http://127.0.0.1:9", Token: "label-token", ElevateLiveTV: true, HideLiveChannels: []string{"99"}},
		"no elevation":    {Upstream: "http://127.0.0.1:9", Token: "label-token", OwnerToken: "owner-token", HideLiveChannels: []string{"99"}},
		"only separators": {Upstream: "http://127.0.0.1:9", Token: "label-token", HideLiveChannels: []string{" , "}},
	} {
		_, err := New(cfg)
		if name == "only separators" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "HideLiveChannels") {
			t.Errorf("%s: err %v, want the HideLiveChannels check", name, err)
		}
	}
	if _, err := New(Config{Upstream: "http://127.0.0.1:9", Token: "label-token", OwnerToken: "owner-token", ElevateAll: true, HideLiveChannels: []string{"99"}}); err != nil {
		t.Errorf("with -elevate-all: %v", err)
	}
}

func TestHideLiveChannel_LeavesResponsesWithoutItAlone(t *testing.T) {
	f := newHideFixture(t)
	p, base := f.proxy(t, nil), f.baseline(t)
	for _, format := range []string{"json", "xml"} {
		path := hideProvider + "/grid?clean=1"
		want := hideServe(base, http.MethodGet, path, "friend-token", format)
		got := hideServe(p, http.MethodGet, path, "friend-token", format)
		if !bytes.Equal(got.Body.Bytes(), want.Body.Bytes()) {
			t.Errorf("%s: a response without the channel changed:\n%s\nwant:\n%s", format, got.Body, want.Body)
		}
		if got.Header().Get("Cache-Control") != "" || got.Header().Get("X-Plex-Content-Original-Length") == "" {
			t.Errorf("%s: headers changed: %v", format, got.Header())
		}
	}
	if strings.Contains(f.logs.String(), "plexlabelproxy_hide: outcome=") {
		t.Errorf("hide outcome logged for an untouched response:\n%s", f.logs.String())
	}
}

func TestHideLiveChannel_SkipsAnEmptyBody(t *testing.T) {
	f := newHideFixture(t)
	p := f.proxy(t, nil)
	if rec := hideServe(p, http.MethodGet, "/livetv/empty", "friend-token", "json"); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body)
	}
	if strings.Contains(f.logs.String(), "plexlabelproxy_hide: outcome=") {
		t.Errorf("an empty body was judged:\n%s", f.logs.String())
	}
}

func TestHideLiveChannel_FlagsAShapeItDoesntKnow(t *testing.T) {
	f := newHideFixture(t)
	p := f.proxy(t, nil)
	if body := hideBody(t, hideServe(p, http.MethodGet, "/livetv/odd", "friend-token", "json")); !bytes.Contains(body, []byte(hiddenIdent)) {
		t.Fatalf("an unknown shape was changed:\n%s", body)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, `plexlabelproxy_hide: outcome=leak_suspect path=/livetv/odd viewer=friend match="`+hiddenIdent+`"`) {
		t.Errorf("no leak_suspect line:\n%s", logs)
	}
	p.emitAuditSummary()
	if logs := f.logs.String(); !strings.Contains(logs, "hidden_leak_suspect=1") {
		t.Errorf("leak not counted:\n%s", logs)
	}
}

func TestHideLiveChannel_DebugCountsWhatTheOwnerKeeps(t *testing.T) {
	f := newHideFixture(t)
	p := f.proxy(t, func(c *Config) { c.HideLiveChannelsDebug = true })
	base := f.baseline(t)
	for _, format := range []string{"json", "xml"} {
		want := hideBody(t, hideServe(base, http.MethodGet, hideProvider+"/grid", "owner-token", format))
		if got := hideBody(t, hideServe(p, http.MethodGet, hideProvider+"/grid", "owner-token", format)); !bytes.Equal(got, want) {
			t.Errorf("%s: debug changed the owner's response", format)
		}
	}
	// Two airings on 99 and their two Channel tags, in each format.
	if n := strings.Count(f.logs.String(), "plexlabelproxy_hide: outcome=owner_kept path="+hideProvider+"/grid viewer=owner present=4"); n != 2 {
		t.Errorf("%d owner_kept lines, want 2:\n%s", n, f.logs.String())
	}
}

func TestTunarrChannelID(t *testing.T) {
	for number, want := range map[string]string{
		"99":  hiddenIdent,
		"1":   "C1.49.tunarr.com",
		"2":   "C2.50.tunarr.com",
		"10":  "C10.97.tunarr.com",
		"2.1": "",
		"05":  "",
		"0":   "",
	} {
		if got := tunarrChannelID(number); got != want {
			t.Errorf("tunarrChannelID(%q) = %q, want %q", number, got, want)
		}
	}
}

func TestParseHiddenChannelValues(t *testing.T) {
	got := parseHiddenChannelValues([]string{"99, C5.53.tunarr.com", "99", "", " , "})
	if want := []string{"99", "C5.53.tunarr.com"}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	h := newHiddenChannels(got, nil)
	for _, id := range []string{"99", hiddenIdent, "C5.53.tunarr.com"} {
		if !h.channelHidden(id) {
			t.Errorf("%s not hidden", id)
		}
	}
	if h.channelHidden("9") || h.channelHidden("C9.57.tunarr.com") {
		t.Error("channel 9 hidden")
	}
}
