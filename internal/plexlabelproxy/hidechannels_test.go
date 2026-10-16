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
}

// hideFixture is a PMS answering with the fixtures (JSON when asked for, XML
// otherwise, gzipped when gzip is set) and a plex.tv whose share list has one
// shared user, friend-token. owner-device-token is one of the owner's apps.
type hideFixture struct {
	pms, plexTV      *httptest.Server
	gzip             atomic.Bool
	plexTVDown       atomic.Bool
	friendUnshared   atomic.Bool
	shareListFetches atomic.Int32
	logs             syncBuffer

	mu    sync.Mutex
	calls []pmsCall
	// dvrsXML, when set, replaces the /livetv/dvrs fixture.
	dvrsXML string
}

type pmsCall struct {
	path, viewAs string
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
	f.calls = append(f.calls, pmsCall{path: r.URL.Path, viewAs: r.Header.Get(ViewAsHeader)})
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
	return p
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
		{name: "hubs", path: hideProvider + "/hubs/discover", size: "1",
			entries: map[string]int{"Hub": 1}, hubSizes: []string{"1"}, kept: []string{"On Cartoons"}},
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

func TestHideLiveChannel_OwnerAndOwnerDevicesSeeEverything(t *testing.T) {
	f := newHideFixture(t)
	p, base := f.proxy(t, nil), f.baseline(t)
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
	// plex.tv is back: the next fetch settles it as the owner's device.
	f.plexTVDown.Store(false)
	p.viewers.mu.Lock()
	p.viewers.retryAfter = 0
	p.viewers.mu.Unlock()
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

func TestLiveViewerTracker_ClassifyBoundsWhatItRemembers(t *testing.T) {
	tr := newLiveViewerTracker(func(context.Context) (shareList, error) { return shareList{}, nil })
	t0 := time.Unix(1_800_000_000, 0)
	for i := 0; i < maxTrackedTokens+50; i++ {
		tr.classify(fmt.Sprintf("fp-%d", i), t0.Add(time.Duration(i)*time.Millisecond))
	}
	tr.mu.Lock()
	n := len(tr.firstSeen)
	tr.mu.Unlock()
	if n > maxTrackedTokens {
		t.Fatalf("remembers %d tokens, want at most %d", n, maxTrackedTokens)
	}
	tr.classify("fp-late", t0.Add(trackedTokenTTL+time.Hour))
	tr.mu.Lock()
	n = len(tr.firstSeen)
	tr.mu.Unlock()
	if n != 1 {
		t.Fatalf("remembers %d tokens after a day, want 1", n)
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
		"99":  "C99.114.tunarr.com",
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
