package plexlabelproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// liveViewersFixture is a PMS (identity, tune, provider list) and a plex.tv
// whose share list has one shared user, "friend" (account 42), with friend-token.
type liveViewersFixture struct {
	pms, plexTV      *httptest.Server
	pathsMu          sync.Mutex
	upstreamPaths    []string
	shareListFetches atomic.Int32
	plexTVDown       atomic.Bool
	newFriendShared  atomic.Bool
	logs             syncBuffer
	viewerLogged     chan struct{}
}

func newLiveViewersFixture(t *testing.T) *liveViewersFixture {
	t.Helper()
	f := &liveViewersFixture{viewerLogged: make(chan struct{}, 64)}
	f.pms = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.pathsMu.Lock()
		f.upstreamPaths = append(f.upstreamPaths, r.URL.Path)
		f.pathsMu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Path == "/identity" {
			if r.Header.Get("X-Plex-Token") != "owner-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`<MediaContainer machineIdentifier="machine-1"/>`))
			return
		}
		if strings.Contains(r.URL.Path, "/channels/fail/") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`<MediaContainer/>`))
	}))
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
		extra := ""
		if f.newFriendShared.Load() {
			extra = `<SharedServer id="3" username="newfriend" userID="44" accessToken="new-friend-token"/>`
		}
		_, _ = w.Write([]byte(`<MediaContainer>
  <SharedServer id="1" username="friend" userID="42" accessToken="friend-token"/>
  <SharedServer id="2" username="pending" userID="43" accessToken=""/>` + extra + `
</MediaContainer>`))
	}))
	t.Cleanup(f.plexTV.Close)
	return f
}

func (f *liveViewersFixture) proxy(t *testing.T, liveViewers bool) *Proxy {
	t.Helper()
	p, err := New(Config{
		Upstream:        f.pms.URL,
		Token:           "label-token",
		OwnerToken:      "owner-token",
		ElevateLiveTV:   true,
		TokenAuthorizer: staticTokenAuthorizer{"friend-token": true, "owner-device-token": true, "new-friend-token": true},
		Logger:          log.New(&f.logs, "", 0),
		LiveViewers:     liveViewers,
		PlexTVURL:       f.plexTV.URL,
	})
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	p.afterViewerLog = func() { f.viewerLogged <- struct{}{} }
	return p
}

// waitViewerLogs waits for n new-viewer log lines, which are written off the
// response path.
func (f *liveViewersFixture) waitViewerLogs(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-f.viewerLogged:
		case <-time.After(5 * time.Second):
			t.Fatalf("waited for %d viewer log line(s), got %d: %s", n, i, f.logs.String())
		}
	}
}

// paths returns the paths PMS has seen; resetPaths forgets them.
func (f *liveViewersFixture) paths() []string {
	f.pathsMu.Lock()
	defer f.pathsMu.Unlock()
	return append([]string(nil), f.upstreamPaths...)
}

func (f *liveViewersFixture) resetPaths() {
	f.pathsMu.Lock()
	f.upstreamPaths = nil
	f.pathsMu.Unlock()
}

// syncBuffer is a bytes.Buffer safe for the proxy's background log writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func tune(p *Proxy, token, client string) {
	req := httptest.NewRequest(http.MethodPost, "/livetv/dvrs/4/channels/C1.49.tunarr.com/tune?X-Plex-Token="+token, nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Plex-Client-Identifier", client)
	p.ServeHTTP(httptest.NewRecorder(), req)
}

func getViewers(t *testing.T, p *Proxy, token string) (*httptest.ResponseRecorder, []LiveViewer) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, LiveViewersPath, nil)
	if token != "" {
		req.Header.Set("X-Plex-Token", token)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	var body struct {
		Viewers []LiveViewer `json:"viewers"`
	}
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
	}
	return rec, body.Viewers
}

func TestProxy_LiveViewersNamesTheSharedUserBehindAClient(t *testing.T) {
	f := newLiveViewersFixture(t)
	p := f.proxy(t, true)
	// The owner device's line waits for a share list fetched after it was first
	// seen, at most every retryAfter (a minute in production).
	p.viewers.retryAfter = 50 * time.Millisecond

	tune(p, "friend-token", "friend-client")
	tune(p, "owner-device-token", "owner-phone")
	tune(p, "owner-token", "owner-server-token-client") // already the owner: not elevated, not listed

	rec, viewers := getViewers(t, p, "owner-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"machineIdentifier":"machine-1"`) {
		t.Fatalf("response doesn't name the server it's for: %s", rec.Body.String())
	}
	got := map[string]LiveViewer{}
	for _, v := range viewers {
		got[v.ClientIdentifier] = v
	}
	if len(got) != 2 {
		t.Fatalf("viewers = %+v, want friend-client and owner-phone", viewers)
	}
	if v := got["friend-client"]; v.PlexAccountID != "42" || v.Username != "friend" || v.LastSeen.IsZero() {
		t.Fatalf("friend-client = %+v, want account 42 (friend)", v)
	}
	if v := got["owner-phone"]; v.PlexAccountID != "" || v.Username != "" {
		t.Fatalf("owner-phone = %+v, want no account (not a shared user's token)", v)
	}
	for _, secret := range []string{"friend-token", "owner-device-token", "owner-token"} {
		if strings.Contains(rec.Body.String(), secret) || strings.Contains(f.logs.String(), secret) {
			t.Fatalf("a token leaked into the response or logs: %s / %s", rec.Body.String(), f.logs.String())
		}
	}
	f.waitViewerLogs(t, 2)
	logged := f.logs.String()
	// Logged before anyone read the list: the share list is fetched for the log line itself.
	if !strings.Contains(logged, `outcome=live_tv_client client="friend-c…" account="friend"`) {
		t.Fatalf("expected the friend's viewer log line with the account resolved, got %s", logged)
	}
	if !strings.Contains(logged, `outcome=live_tv_client client="owner-ph…" account="unknown"`) {
		t.Fatalf("expected the owner device's viewer log line, got %s", logged)
	}
}

func TestProxy_LiveViewersOnlyRecordsLiveTVStarts(t *testing.T) {
	f := newLiveViewersFixture(t)
	p := f.proxy(t, true)

	// Browsing the guide is elevated too, but isn't watching.
	req := httptest.NewRequest(http.MethodGet, "/media/providers?X-Plex-Token=friend-token", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Plex-Client-Identifier", "browsing-client")
	p.ServeHTTP(httptest.NewRecorder(), req)

	if _, viewers := getViewers(t, p, "owner-token"); len(viewers) != 0 {
		t.Fatalf("viewers = %+v, want none after only browsing", viewers)
	}
}

func TestProxy_LiveViewersNeedsTheOwnerToken(t *testing.T) {
	f := newLiveViewersFixture(t)
	p := f.proxy(t, true)
	tune(p, "friend-token", "friend-client")
	f.resetPaths()

	for _, token := range []string{"", "friend-token", "owner-token-but-longer"} {
		if rec, _ := getViewers(t, p, token); rec.Code != http.StatusNotFound {
			t.Fatalf("token %q: status=%d, want 404", token, rec.Code)
		}
	}
	for _, path := range f.paths() {
		if path == LiveViewersPath {
			t.Fatalf("the viewers path was proxied to PMS")
		}
	}
	if f.shareListFetches.Load() != 0 {
		t.Fatalf("unauthenticated requests fetched the share list %d times", f.shareListFetches.Load())
	}
}

func TestProxy_LiveViewersOffByDefault(t *testing.T) {
	f := newLiveViewersFixture(t)
	p := f.proxy(t, false)
	tune(p, "friend-token", "friend-client")

	req := httptest.NewRequest(http.MethodGet, LiveViewersPath, nil)
	req.Header.Set("X-Plex-Token", "owner-token")
	p.ServeHTTP(httptest.NewRecorder(), req)
	if paths := f.paths(); len(paths) == 0 || paths[len(paths)-1] != LiveViewersPath {
		t.Fatalf("with LiveViewers off the path should reach PMS like any other; PMS saw %v", f.paths())
	}
	if strings.Contains(f.logs.String(), "plexlabelproxy_viewer") {
		t.Fatalf("viewer tracking ran while off: %s", f.logs.String())
	}
}

func TestLiveViewerTracker_ShareListRefresh(t *testing.T) {
	var fetches int
	accounts := map[string]plexAccount{}
	tr := newLiveViewerTracker(func(_ context.Context) (shareList, error) {
		fetches++
		out := map[string]plexAccount{}
		for k, v := range accounts {
			out[k] = v
		}
		return shareList{accounts: out, machineIdentifier: "m"}, nil
	})
	ctx := context.Background()
	t0 := time.Unix(1_800_000_000, 0)

	tr.record("owner-phone", "fp-owner", t0)
	_ = tr.refreshAccounts(ctx, t0)
	if fetches != 1 {
		t.Fatalf("first refresh fetched %d times, want 1", fetches)
	}
	// An unknown token that was already unknown at the last fetch doesn't
	// cause a fetch per minute.
	_ = tr.refreshAccounts(ctx, t0.Add(2*time.Minute))
	if fetches != 1 {
		t.Fatalf("an already-unknown token refetched: %d fetches", fetches)
	}
	// A new client with an unknown token (a friend added since) refetches
	// after accountsRetry, not before.
	accounts["fp-new-friend"] = plexAccount{id: "77", username: "new"}
	tr.record("new-client", "fp-new-friend", t0.Add(2*time.Minute))
	_ = tr.refreshAccounts(ctx, t0.Add(2*time.Minute))
	if fetches != 2 {
		t.Fatalf("a newly seen unknown token didn't refetch: %d fetches", fetches)
	}
	if a, ok := tr.account("fp-new-friend"); !ok || a.id != "77" {
		t.Fatalf("new friend not resolved: %+v %v", a, ok)
	}
	// The list goes stale after accountsMaxAge.
	_ = tr.refreshAccounts(ctx, t0.Add(2*time.Minute+accountsMaxAge+time.Second))
	if fetches != 3 {
		t.Fatalf("stale list not refetched: %d fetches", fetches)
	}
	// Clients are forgotten after liveViewerTTL.
	if got := tr.list(t0.Add(liveViewerTTL + 3*time.Minute)); len(got) != 0 {
		t.Fatalf("expired clients still listed: %+v", got)
	}
}

func TestProxy_LiveViewersKey(t *testing.T) {
	f := newLiveViewersFixture(t)
	key := "a-long-random-viewers-key-0123456789"
	newProxy := func(key string) *Proxy {
		p, err := New(Config{
			Upstream:        f.pms.URL,
			Token:           "label-token",
			OwnerToken:      "owner-token",
			ElevateLiveTV:   true,
			TokenAuthorizer: staticTokenAuthorizer{"friend-token": true},
			Logger:          log.New(&f.logs, "", 0),
			LiveViewers:     true,
			LiveViewersKey:  key,
			PlexTVURL:       f.plexTV.URL,
		})
		if err != nil {
			t.Fatalf("new proxy: %v", err)
		}
		return p
	}
	withAuth := func(p *Proxy, auth string) int {
		req := httptest.NewRequest(http.MethodGet, LiveViewersPath, nil)
		req.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec.Code
	}

	p := newProxy(key)
	tune(p, "friend-token", "friend-client")
	if code := withAuth(p, "Bearer "+key); code != http.StatusOK {
		t.Fatalf("right key: status=%d, want 200", code)
	}
	if code := withAuth(p, "bearer "+key); code != http.StatusOK {
		t.Fatalf("lower-case scheme: status=%d, want 200", code)
	}
	for _, auth := range []string{"Bearer wrong-key-wrong-key-wrong-key-00", "Bearer " + key + "x", key, "Basic " + key} {
		if code := withAuth(p, auth); code != http.StatusNotFound {
			t.Fatalf("auth %q: status=%d, want 404", auth, code)
		}
	}

	// The key is no Plex token: sent as one, it elevates nothing.
	f.resetPaths()
	req := httptest.NewRequest(http.MethodGet, "/livetv/dvrs?X-Plex-Token="+key, nil)
	req.RemoteAddr = "127.0.0.1:12345"
	p.ServeHTTP(httptest.NewRecorder(), req)
	if strings.Contains(f.logs.String(), "outcome=elevated_live_tv method=GET path=/livetv/dvrs ") {
		t.Fatalf("the viewers key was elevated as a Plex token: %s", f.logs.String())
	}

	// A key shorter than the minimum is ignored.
	short := newProxy("short-key")
	if code := withAuth(short, "Bearer short-key"); code != http.StatusNotFound {
		t.Fatalf("short key: status=%d, want 404", code)
	}
}

func TestProxy_LiveViewersOnlyRecordsStartsPMSAccepted(t *testing.T) {
	f := newLiveViewersFixture(t)
	p := f.proxy(t, true)

	req := httptest.NewRequest(http.MethodPost, "/livetv/dvrs/4/channels/fail/tune?X-Plex-Token=friend-token", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Plex-Client-Identifier", "refused-client")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("tune status=%d, want PMS's 500 passed through", rec.Code)
	}
	if _, viewers := getViewers(t, p, "owner-token"); len(viewers) != 0 {
		t.Fatalf("a start PMS refused was recorded: %+v", viewers)
	}

	// PMS unreachable: the proxy answers 502 and records nothing either.
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()
	unreachable, err := New(Config{
		Upstream:        downURL,
		Token:           "label-token",
		OwnerToken:      "owner-token",
		ElevateLiveTV:   true,
		TokenAuthorizer: staticTokenAuthorizer{"friend-token": true},
		Logger:          log.New(&f.logs, "", 0),
		LiveViewers:     true,
		PlexTVURL:       f.plexTV.URL,
	})
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	tune(unreachable, "friend-token", "unreachable-client")
	if _, viewers := getViewers(t, unreachable, "owner-token"); len(viewers) != 0 {
		t.Fatalf("a start that never reached PMS was recorded: %+v", viewers)
	}
}

func TestProxy_LiveViewerClientIdentifiersCantForgeLogLines(t *testing.T) {
	f := newLiveViewersFixture(t)
	p := f.proxy(t, true)

	// A control character makes the identifier invalid: not recorded, nothing logged.
	req := httptest.NewRequest(http.MethodPost, "/livetv/dvrs/4/channels/C1.49.tunarr.com/tune?X-Plex-Token=friend-token&clientIdentifier=a%0Aforged", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	p.ServeHTTP(httptest.NewRecorder(), req)
	if _, viewers := getViewers(t, p, "owner-token"); len(viewers) != 0 {
		t.Fatalf("an identifier with a newline was recorded: %+v", viewers)
	}

	// A printable identifier is logged quoted.
	tune(p, "friend-token", `q"uote`)
	f.waitViewerLogs(t, 1)
	logged := f.logs.String()
	for _, line := range strings.Split(logged, "\n") {
		if strings.HasPrefix(line, "forged") {
			t.Fatalf("a client identifier forged a log line: %q", logged)
		}
	}
	if !strings.Contains(logged, `client="q\"uote"`) {
		t.Fatalf("expected the quoted identifier in the viewer log, got %s", logged)
	}
}

func TestProxy_LiveViewersBoundsClientIdentifiers(t *testing.T) {
	f := newLiveViewersFixture(t)
	p := f.proxy(t, true)

	longest := strings.Repeat("c", maxClientIDLen)
	tune(p, "friend-token", longest)
	tune(p, "friend-token", longest+"x")
	tune(p, "friend-token", "tab\there")
	f.waitViewerLogs(t, 1)

	_, viewers := getViewers(t, p, "owner-token")
	if len(viewers) != 1 || viewers[0].ClientIdentifier != longest {
		t.Fatalf("viewers = %+v, want only the %d-character identifier", viewers, maxClientIDLen)
	}
}

func TestProxy_LiveViewersOwnerStartClearsAStaleMapping(t *testing.T) {
	f := newLiveViewersFixture(t)
	p := f.proxy(t, true)

	tune(p, "friend-token", "shared-tv")
	f.waitViewerLogs(t, 1)
	if _, viewers := getViewers(t, p, "owner-token"); len(viewers) != 1 {
		t.Fatalf("viewers = %+v, want the friend's client", viewers)
	}
	// The same app, now signed in as the owner (with the owner token itself).
	tune(p, "owner-token", "shared-tv")
	if _, viewers := getViewers(t, p, "owner-token"); len(viewers) != 0 {
		t.Fatalf("viewers = %+v, want the stale friend mapping gone", viewers)
	}
}

func TestLiveViewerTracker_ConcurrentRefreshesShareOneFetch(t *testing.T) {
	var fetches atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	tr := newLiveViewerTracker(func(_ context.Context) (shareList, error) {
		fetches.Add(1)
		close(started)
		<-release
		return shareList{accounts: map[string]plexAccount{"fp-friend": {id: "42", username: "friend"}}}, nil
	})
	now := time.Unix(1_800_000_000, 0)
	tr.record("client-a", "fp-friend", now)
	tr.record("client-b", "fp-friend", now)

	first := make(chan struct{})
	go func() {
		_ = tr.refreshAccounts(context.Background(), now)
		close(first)
	}()
	<-started
	second := make(chan struct{})
	go func() {
		_ = tr.refreshAccounts(context.Background(), now)
		close(second)
	}()
	select {
	case <-second:
		t.Fatal("the second refresh returned before the fetch in flight finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-first
	<-second
	if a, ok := tr.account("fp-friend"); !ok || a.username != "friend" {
		t.Fatalf("after both refreshes the account isn't known: %+v %v", a, ok)
	}
	if fetches.Load() != 1 {
		t.Fatalf("fetches = %d, want 1", fetches.Load())
	}
}

func TestProxy_LiveViewerLogWaitsOutAPlexTVFailure(t *testing.T) {
	f := newLiveViewersFixture(t)
	p := f.proxy(t, true)
	p.viewers.retryAfter = 0

	f.plexTVDown.Store(true)
	tune(p, "friend-token", "friend-client")
	f.waitViewerLogs(t, 1) // the attempt, which writes no viewer line
	if strings.Contains(f.logs.String(), "outcome=live_tv_client") {
		t.Fatalf("logged the client while plex.tv was failing: %s", f.logs.String())
	}
	if !strings.Contains(f.logs.String(), "share list fetch failed") {
		t.Fatalf("expected the fetch failure to be logged, got %s", f.logs.String())
	}

	// plex.tv is back; the client's next Live TV start writes its line, named.
	f.plexTVDown.Store(false)
	tune(p, "friend-token", "friend-client")
	f.waitViewerLogs(t, 1)
	if !strings.Contains(f.logs.String(), `outcome=live_tv_client client="friend-c…" account="friend"`) {
		t.Fatalf("expected the named viewer line after plex.tv recovered, got %s", f.logs.String())
	}
	// And only once.
	tune(p, "friend-token", "friend-client")
	if n := strings.Count(f.logs.String(), "outcome=live_tv_client"); n != 1 {
		t.Fatalf("viewer lines = %d, want 1", n)
	}
}

func TestProxy_LiveViewerLogWaitsForAListNewerThanTheClient(t *testing.T) {
	f := newLiveViewersFixture(t)
	p := f.proxy(t, true)
	p.viewers.retryAfter = 100 * time.Millisecond

	// A successful fetch just now, before the friend was shared.
	getViewers(t, p, "owner-token")
	f.newFriendShared.Store(true)

	// Their first start comes inside the retry window: the list in hand predates
	// them, so they aren't written off as an owner device; the retry names them.
	tune(p, "new-friend-token", "new-friend-tv")
	f.waitViewerLogs(t, 1)
	if !strings.Contains(f.logs.String(), `client="new-frie…" account="newfriend"`) {
		t.Fatalf("expected the new friend named after the retry, got %s", f.logs.String())
	}
}

func TestProxy_LiveViewerLookupsAreBounded(t *testing.T) {
	f := newLiveViewersFixture(t)
	p := f.proxy(t, true)
	// Every lookup slot is busy.
	for i := 0; i < maxViewerLoggers; i++ {
		p.viewers.loggers <- struct{}{}
	}
	tune(p, "friend-token", "burst-client")
	select {
	case <-f.viewerLogged:
		t.Fatal("a lookup ran with every slot taken")
	case <-time.After(100 * time.Millisecond):
	}
	// The client is still listed, and its line comes with its next start once a slot frees.
	if _, viewers := getViewers(t, p, "owner-token"); len(viewers) != 1 {
		t.Fatalf("viewers = %+v, want the client listed", viewers)
	}
	<-p.viewers.loggers
	tune(p, "friend-token", "burst-client")
	f.waitViewerLogs(t, 1)
	if !strings.Contains(f.logs.String(), `client="burst-cl…" account="friend"`) {
		t.Fatalf("expected the deferred line, got %s", f.logs.String())
	}
}
