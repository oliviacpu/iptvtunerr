package plexlabelproxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// LiveViewersPath serves, to callers holding the owner token, which Plex
// account is behind each client that started Live TV through the proxy.
//
// Plex records an elevated Live TV session under the owner, because the proxy
// tuned it with the owner token, so monitoring tools (Tracearr, Tautulli) show
// every shared user's Live TV as the owner's. They can ask here instead: the
// session's Player.machineIdentifier is the client identifier listed.
const LiveViewersPath = "/iptvtunerr/live-viewers"

const (
	// liveViewerTTL is how long a client is remembered after its last Live TV start.
	liveViewerTTL = 12 * time.Hour
	// maxLiveViewers bounds the table; the least recently seen client goes first.
	maxLiveViewers = 1000
	// accountsMaxAge is how old the share list may get before it's fetched again,
	// and accountsRetry how soon a failed or incomplete fetch is retried.
	accountsMaxAge = 10 * time.Minute
	accountsRetry  = time.Minute
)

// LiveViewer is one client in the LiveViewersPath response. PlexAccountID and
// Username are empty when the client's token isn't a shared user's token for
// this server (in practice, one of the owner's own devices).
type LiveViewer struct {
	ClientIdentifier string    `json:"clientIdentifier"`
	PlexAccountID    string    `json:"plexAccountId"`
	Username         string    `json:"username"`
	LastSeen         time.Time `json:"lastSeen"`
}

type plexAccount struct {
	id       string
	username string
}

type liveViewerEntry struct {
	tokenFP  string
	lastSeen time.Time
	// since is when this client was first seen with this token.
	since time.Time
	// logged: its one log line is written; logging: that line is being
	// resolved (account lookup) right now.
	logged, logging bool
}

// liveViewerTracker keeps client identifier -> token fingerprint for Live TV
// starts, and token fingerprint -> account from plex.tv's share list for this
// server. Only fingerprints are kept; shared users' tokens are hashed as the
// list is read and never stored.
type liveViewerTracker struct {
	fetchAccounts func(ctx context.Context) (shareList, error)

	mu        sync.Mutex
	clients   map[string]liveViewerEntry
	accounts  map[string]plexAccount
	machineID string
	fetchedAt time.Time
	triedAt   time.Time
	// refreshing is closed when the share-list fetch in flight finishes.
	refreshing chan struct{}
	// retryAfter is how soon a failed or incomplete fetch is retried
	// (accountsRetry; tests shorten it).
	retryAfter time.Duration
	// lastFetchOK: the most recent share-list fetch succeeded; lastOKAt is
	// when that successful fetch finished (wall clock).
	lastFetchOK bool
	lastOKAt    time.Time
	// loggers holds a slot per running new-viewer lookup (maxViewerLoggers).
	loggers chan struct{}

	// For -hide-live-channel (classify): friends holds every token
	// fingerprint a share list has named, so a later list that lacks it (an
	// empty or failed answer from plex.tv) can't make a friend look like one
	// of the owner's devices. seen is when a token was first classified, and
	// last classified (which keeps it remembered).
	// classifiedAt is when the last share list trusted for classification was
	// fetched (its start); a list that comes back empty right after a non-empty
	// one isn't trusted until a second one agrees (emptyOnce).
	friends      map[string]time.Time
	seen         map[string]tokenSeen
	classifiedAt time.Time
	lastListSize int
	emptyOnce    bool
}

// shareList is plex.tv's share list for the server: shared users by token
// fingerprint, and the server's machine identifier it was fetched for.
type shareList struct {
	accounts          map[string]plexAccount
	machineIdentifier string
}

func newLiveViewerTracker(fetch func(ctx context.Context) (shareList, error)) *liveViewerTracker {
	return &liveViewerTracker{
		fetchAccounts: fetch,
		clients:       make(map[string]liveViewerEntry),
		accounts:      make(map[string]plexAccount),
		retryAfter:    accountsRetry,
		loggers:       make(chan struct{}, maxViewerLoggers),
		friends:       make(map[string]time.Time),
		seen:          make(map[string]tokenSeen),
	}
}

// record notes that clientID started Live TV with the token whose fingerprint
// is tokenFP. It reports whether the caller should write the client's log
// line: the client is new (or has a new token), or its line is still unwritten
// because its account couldn't be settled yet, and nobody is writing it now.
func (t *liveViewerTracker) record(clientID, tokenFP string, now time.Time) bool {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" || tokenFP == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	prev, known := t.clients[clientID]
	e := prev
	if !known || prev.tokenFP != tokenFP {
		e = liveViewerEntry{tokenFP: tokenFP, since: now}
	}
	e.lastSeen = now
	log := !e.logged && !e.logging
	if log {
		e.logging = true
	}
	t.clients[clientID] = e
	t.pruneLocked(now)
	return log
}

// settle reports the account behind clientID's token for its log line, and
// whether that's final: the account is known, or a share list fetched
// successfully after the client was first seen doesn't have it (one of the
// owner's devices). Not final (no such list yet, or plex.tv failed), the
// caller retries.
func (t *liveViewerTracker) settle(clientID, tokenFP string) (who string, final bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.clients[clientID]
	if !ok || e.tokenFP != tokenFP {
		return "", false
	}
	if a, known := t.accounts[tokenFP]; known {
		return a.username, true
	}
	if t.lastFetchOK && !t.lastOKAt.Before(e.since) {
		return "unknown", true
	}
	return "", false
}

// finishLog ends the logging of clientID's line: logged when it was written,
// otherwise left for the client's next Live TV start.
func (t *liveViewerTracker) finishLog(clientID, tokenFP string, logged bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.clients[clientID]
	if !ok || e.tokenFP != tokenFP {
		return
	}
	e.logging = false
	e.logged = logged
	t.clients[clientID] = e
}

func (t *liveViewerTracker) pruneLocked(now time.Time) {
	for id, e := range t.clients {
		if now.Sub(e.lastSeen) > liveViewerTTL {
			delete(t.clients, id)
		}
	}
	for len(t.clients) > maxLiveViewers {
		oldest, oldestAt := "", now
		for id, e := range t.clients {
			if oldest == "" || e.lastSeen.Before(oldestAt) {
				oldest, oldestAt = id, e.lastSeen
			}
		}
		delete(t.clients, oldest)
	}
}

// forget drops clientID, for a client now used with the owner token.
func (t *liveViewerTracker) forget(clientID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.clients, clientID)
}

// machineIdentifier is the PMS machine identifier of the last share list.
func (t *liveViewerTracker) machineIdentifier() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.machineID
}

// account returns the shared user behind tokenFP from the last share list.
func (t *liveViewerTracker) account(tokenFP string) (plexAccount, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a, ok := t.accounts[tokenFP]
	return a, ok
}

// refreshAccounts fetches the share list when it's older than accountsMaxAge,
// or sooner (at most every accountsRetry) when a client first seen since the
// last fetch has a token that isn't in it, such as a friend added since then.
// A token still missing after a fetch (the owner's own devices) waits for the
// regular refresh. While a fetch is in flight, callers wait for it (or for
// ctx) instead of starting another, so they all see its result.
func (t *liveViewerTracker) refreshAccounts(ctx context.Context, now time.Time) error {
	t.mu.Lock()
	if wait := t.refreshing; wait != nil {
		t.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
		}
		return nil
	}
	// An empty share list that isn't trusted yet is asked again soon.
	missing := t.emptyOnce
	for _, e := range t.clients {
		if _, ok := t.accounts[e.tokenFP]; !ok && e.since.After(t.fetchedAt) {
			missing = true
			break
		}
	}
	// A token classify hasn't settled yet (first seen since the last fetch)
	// needs a fresh list too.
	for fp, seen := range t.seen {
		if missing {
			break
		}
		_, shared := t.accounts[fp]
		_, friend := t.friends[fp]
		if !shared && !friend && seen.first.After(t.fetchedAt) {
			missing = true
		}
	}
	stale := t.fetchedAt.IsZero() || now.Sub(t.fetchedAt) > accountsMaxAge
	due := (stale || missing) && now.Sub(t.triedAt) >= t.retryAfter
	if !due {
		t.mu.Unlock()
		return nil
	}
	t.triedAt = now
	done := make(chan struct{})
	t.refreshing = done
	t.mu.Unlock()

	list, err := t.fetchAccounts(ctx)
	t.mu.Lock()
	t.lastFetchOK = err == nil
	if err == nil {
		t.lastOKAt = time.Now()
		t.accounts = list.accounts
		t.machineID = list.machineIdentifier
		t.fetchedAt = now
		for fp := range list.accounts {
			t.friends[fp] = now
		}
		t.pruneTokensLocked(now)
		// A list that's suddenly empty may be plex.tv misbehaving: trusting it
		// would make every newly seen friend look like an owner device. A
		// second empty list in a row is believed (everyone was unshared).
		if len(list.accounts) == 0 && t.lastListSize > 0 && !t.emptyOnce {
			t.emptyOnce = true
		} else {
			t.emptyOnce = false
			t.lastListSize = len(list.accounts)
			t.classifiedAt = now
		}
	}
	t.refreshing = nil
	t.mu.Unlock()
	close(done)
	return err
}

// viewerClass is who a Live TV caller is, for -hide-live-channel.
type viewerClass int

const (
	// viewerUnsettled: not known yet (no share list fetched since the token
	// was first seen, or no token). Treated like a friend.
	viewerUnsettled viewerClass = iota
	// viewerFriend: the token is on the server's share list (now or before).
	viewerFriend
	// viewerOwnerDevice: one of the owner's own apps. Its token isn't on a
	// share list fetched after the token was first seen.
	viewerOwnerDevice
	// viewerOwner: the owner token itself.
	viewerOwner
	// viewerPreview: the owner asking to see a response as a friend would.
	viewerPreview
)

func (c viewerClass) String() string {
	switch c {
	case viewerFriend:
		return "friend"
	case viewerOwnerDevice:
		return "owner_device"
	case viewerOwner:
		return "owner"
	case viewerPreview:
		return "preview"
	}
	return "unsettled"
}

// seesHiddenChannels reports whether this caller sees channels hidden with
// -hide-live-channel: the owner and the owner's devices.
func (c viewerClass) seesHiddenChannels() bool {
	return c == viewerOwner || c == viewerOwnerDevice
}

// maxTrackedTokens bounds the friends and seen tables, and trackedTokenTTL
// is how long a token is kept after it was last noted.
const (
	maxTrackedTokens = 1000
	trackedTokenTTL  = 24 * time.Hour
)

// classify says who the token with fingerprint fp belongs to, and whether
// that's settled. Not settled (a token first seen since the last trusted
// share list), the caller refreshes the list and asks again.
func (t *liveViewerTracker) classify(fp string, now time.Time) (viewerClass, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.accounts[fp]; ok {
		t.friends[fp] = now
		return viewerFriend, true
	}
	if _, ok := t.friends[fp]; ok {
		t.friends[fp] = now
		return viewerFriend, true
	}
	seen, ok := t.seen[fp]
	if !ok {
		seen.first = now
	}
	seen.last = now
	t.seen[fp] = seen
	if !ok {
		t.pruneTokensLocked(now)
	}
	if !t.classifiedAt.IsZero() && !t.classifiedAt.Before(seen.first) {
		return viewerOwnerDevice, true
	}
	return viewerUnsettled, false
}

// tokenSeen is when classify first and last saw a token.
type tokenSeen struct {
	first, last time.Time
}

func (t *liveViewerTracker) pruneTokensLocked(now time.Time) {
	pruneTracked(t.friends, func(at time.Time) time.Time { return at }, now)
	pruneTracked(t.seen, func(s tokenSeen) time.Time { return s.last }, now)
}

// pruneTracked drops the entries of m last noted (lastNoted) over
// trackedTokenTTL ago, then the least recently noted past maxTrackedTokens.
func pruneTracked[V any](m map[string]V, lastNoted func(V) time.Time, now time.Time) {
	for fp, v := range m {
		if now.Sub(lastNoted(v)) > trackedTokenTTL {
			delete(m, fp)
		}
	}
	for len(m) > maxTrackedTokens {
		oldest, oldestAt := "", now
		for fp, v := range m {
			if at := lastNoted(v); oldest == "" || at.Before(oldestAt) {
				oldest, oldestAt = fp, at
			}
		}
		delete(m, oldest)
	}
}

func (t *liveViewerTracker) list(now time.Time) []LiveViewer {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(now)
	out := make([]LiveViewer, 0, len(t.clients))
	for id, e := range t.clients {
		v := LiveViewer{ClientIdentifier: id, LastSeen: e.lastSeen.UTC()}
		if a, ok := t.accounts[e.tokenFP]; ok {
			v.PlexAccountID, v.Username = a.id, a.username
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

// pendingLiveViewerKey carries, on an elevated Live TV start, the client and
// token fingerprint to record once PMS has accepted the start.
type pendingLiveViewerKey struct{}

type pendingLiveViewer struct {
	clientID string
	tokenFP  string
	// owner: the start used the owner token, so any earlier mapping of this
	// client to a shared user is stale.
	owner bool
}

// markLiveViewer notes on an elevated Live TV start which client made it.
// The proxy's Director runs before PMS answers, so the viewer is recorded
// later, by recordAcceptedLiveViewer, and only if PMS accepted the start.
func (p *Proxy) markLiveViewer(req *http.Request, token string) {
	if p.viewers == nil || !p.cfg.LiveViewers || !IsLiveTVStreamRequest(req) {
		return
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	clientID := strings.TrimSpace(firstNonEmpty(req.Header.Get("X-Plex-Client-Identifier"), req.URL.Query().Get("X-Plex-Client-Identifier"), req.URL.Query().Get("clientIdentifier")))
	if !validClientID(clientID) {
		return
	}
	v := pendingLiveViewer{clientID: clientID, owner: token == strings.TrimSpace(p.cfg.OwnerToken)}
	if !v.owner {
		v.tokenFP = tokenFingerprint(token)
	}
	*req = *req.WithContext(context.WithValue(req.Context(), pendingLiveViewerKey{}, v))
}

// maxClientIDLen bounds a remembered client identifier. Plex's are 20-40
// characters; the client sends it, so a longer one isn't kept.
const maxClientIDLen = 128

// validClientID reports whether id is worth remembering: non-empty, at most
// maxClientIDLen bytes, printable ASCII only.
func validClientID(id string) bool {
	if id == "" || len(id) > maxClientIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

// recordAcceptedLiveViewer records the viewer marked on resp's request when
// PMS accepted the start (2xx); a refused or failed start isn't a viewer.
// A newly seen client is logged once its account is known: the share list is
// refreshed first when needed, off the response path.
func (p *Proxy) recordAcceptedLiveViewer(resp *http.Response) {
	if p.viewers == nil || resp == nil || resp.Request == nil || resp.StatusCode < 200 || resp.StatusCode > 299 {
		return
	}
	v, ok := resp.Request.Context().Value(pendingLiveViewerKey{}).(pendingLiveViewer)
	if !ok {
		return
	}
	if v.owner {
		p.viewers.forget(v.clientID)
		return
	}
	if !p.viewers.record(v.clientID, v.tokenFP, time.Now()) {
		return
	}
	// At most maxViewerLoggers lookups at a time (each may wait out the retry
	// gate); past that, the client's line waits for its next Live TV start.
	select {
	case p.viewers.loggers <- struct{}{}:
		go func() {
			defer func() { <-p.viewers.loggers }()
			p.logNewLiveViewer(v)
		}()
	default:
		p.viewers.finishLog(v.clientID, v.tokenFP, false)
	}
}

// viewerLogAttempts is how many times a new client's account lookup is tried
// before leaving it to the client's next Live TV start, and maxViewerLoggers
// how many such lookups may run at once.
const (
	viewerLogAttempts = 3
	maxViewerLoggers  = 16
)

func (p *Proxy) logNewLiveViewer(v pendingLiveViewer) {
	var who string
	final := false
	for attempt := 0; attempt < viewerLogAttempts && !final; attempt++ {
		if attempt > 0 {
			// The share list can be refetched once the retry gate has passed.
			time.Sleep(p.viewers.retryAfter)
		}
		if _, known := p.viewers.account(v.tokenFP); !known {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			if err := p.viewers.refreshAccounts(ctx, time.Now()); err != nil {
				p.logger.Printf("plexlabelproxy_viewer: share list fetch failed: %v", err)
			}
			cancel()
		}
		who, final = p.viewers.settle(v.clientID, v.tokenFP)
	}
	p.viewers.finishLog(v.clientID, v.tokenFP, final)
	if !final {
		// plex.tv couldn't say: the client's next Live TV start tries again.
		if p.afterViewerLog != nil {
			p.afterViewerLog()
		}
		return
	}
	// The client identifier comes from the client: quoted, so it can't forge log lines.
	p.logger.Printf("plexlabelproxy_viewer: outcome=live_tv_client client=%q account=%q token_fp=%s", shortID(v.clientID), who, v.tokenFP)
	if p.afterViewerLog != nil {
		p.afterViewerLog()
	}
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8] + "…"
}

// liveViewersAuthorized reports whether r may read the list: it carries the
// owner token, or Config.LiveViewersKey as "Authorization: Bearer <key>".
// The key lets a monitoring tool read the list without holding the owner
// token.
func (p *Proxy) liveViewersAuthorized(r *http.Request) bool {
	equal := func(got, want string) bool {
		return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
	}
	if equal(strings.TrimSpace(inboundPlexToken(r)), strings.TrimSpace(p.cfg.OwnerToken)) {
		return true
	}
	key := strings.TrimSpace(p.cfg.LiveViewersKey)
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(key) >= minLiveViewersKeyLen && len(auth) > len("bearer ") && strings.EqualFold(auth[:len("bearer ")], "bearer ") {
		return equal(strings.TrimSpace(auth[len("bearer "):]), key)
	}
	return false
}

// minLiveViewersKeyLen is the shortest Config.LiveViewersKey accepted; a
// shorter key is ignored, leaving only the owner token.
const minLiveViewersKeyLen = 24

// serveLiveViewers answers LiveViewersPath. Unauthorized, it answers 404, as
// if the path weren't there.
func (p *Proxy) serveLiveViewers(w http.ResponseWriter, r *http.Request) {
	if !p.liveViewersAuthorized(r) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	now := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := p.viewers.refreshAccounts(ctx, now); err != nil {
		p.logger.Printf("plexlabelproxy_viewer: share list fetch failed: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		// MachineIdentifier is the Plex server the list is for, so a tool
		// watching several servers applies it to this one only.
		MachineIdentifier string       `json:"machineIdentifier"`
		Viewers           []LiveViewer `json:"viewers"`
	}{p.viewers.machineIdentifier(), p.viewers.list(now)})
}

// plexShareFetcher reads this server's share list from plex.tv with the owner
// token: each shared user's account and their access token for this server,
// which is the token their apps send. Tokens are fingerprinted as they're read.
func plexShareFetcher(upstream *url.URL, plexTV, ownerToken string, pmsTransport http.RoundTripper) func(context.Context) (shareList, error) {
	pms := &http.Client{Transport: pmsTransport, Timeout: 10 * time.Second}
	tv := &http.Client{Timeout: 10 * time.Second}
	return func(ctx context.Context) (shareList, error) {
		machine, err := pmsMachineIdentifier(ctx, pms, upstream, ownerToken)
		if err != nil {
			return shareList{}, err
		}
		u := strings.TrimRight(plexTV, "/") + "/api/servers/" + url.PathEscape(machine) + "/shared_servers"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return shareList{}, err
		}
		req.Header.Set("X-Plex-Token", ownerToken)
		req.Header.Set("X-Plex-Client-Identifier", "iptvtunerr-plex-label-proxy")
		req.Header.Set("X-Plex-Product", "iptvtunerr")
		req.Header.Set("Accept", "application/xml")
		resp, err := tv.Do(req)
		if err != nil {
			return shareList{}, fmt.Errorf("plex.tv shared_servers: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return shareList{}, fmt.Errorf("plex.tv shared_servers: status %d", resp.StatusCode)
		}
		var doc struct {
			Shares []struct {
				AccessToken string `xml:"accessToken,attr"`
				UserID      string `xml:"userID,attr"`
				Username    string `xml:"username,attr"`
			} `xml:"SharedServer"`
		}
		if err := xml.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&doc); err != nil {
			return shareList{}, fmt.Errorf("plex.tv shared_servers: %w", err)
		}
		accounts := make(map[string]plexAccount, len(doc.Shares))
		for _, s := range doc.Shares {
			if strings.TrimSpace(s.AccessToken) == "" || strings.TrimSpace(s.UserID) == "" {
				continue
			}
			accounts[tokenFingerprint(s.AccessToken)] = plexAccount{id: s.UserID, username: s.Username}
		}
		return shareList{accounts: accounts, machineIdentifier: machine}, nil
	}
}

func pmsMachineIdentifier(ctx context.Context, client *http.Client, upstream *url.URL, ownerToken string) (string, error) {
	u := *upstream
	u.Path = "/identity"
	u.RawQuery = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Plex-Token", ownerToken)
	req.Header.Set("Accept", "application/xml")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("PMS /identity: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("PMS /identity: status %d", resp.StatusCode)
	}
	var doc struct {
		MachineIdentifier string `xml:"machineIdentifier,attr"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return "", fmt.Errorf("PMS /identity: %w", err)
	}
	if strings.TrimSpace(doc.MachineIdentifier) == "" {
		return "", fmt.Errorf("PMS /identity: no machineIdentifier")
	}
	return doc.MachineIdentifier, nil
}
