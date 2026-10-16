package plexlabelproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// TokenAuthorizer answers whether an inbound Plex token already has access to
// this server. The proxy only borrows the owner token after this check passes.
type TokenAuthorizer interface {
	AllowPlexToken(ctx context.Context, token string) bool
}

type AuthorizationDecision struct {
	Allowed  bool
	CacheHit bool
	// Unavailable means PMS could not answer (unreachable, timed out, or an
	// error status other than 401/403), so the token was neither confirmed
	// nor refused by this check. A refusal for that reason is not the
	// client's fault and must not count as a bad elevation attempt.
	Unavailable bool
}

type DetailedTokenAuthorizer interface {
	AllowPlexTokenDetailed(ctx context.Context, token string) AuthorizationDecision
}

type tokenDecision struct {
	allowed bool
	expires time.Time
}

// PlexTokenAuthorizer validates client tokens against PMS and caches the
// result briefly so Smart TV clients do not pay a validation request on every
// Live TV segment or timeline tick.
type PlexTokenAuthorizer struct {
	upstreamURL *url.URL
	ownerToken  string
	client      *http.Client
	ttl         time.Duration
	// staleFor is how long after PMS last accepted a token it stays allowed
	// while PMS cannot answer; retryAfter is how long such a stale answer is
	// cached before PMS is asked again.
	staleFor   time.Duration
	retryAfter time.Duration

	mu    sync.Mutex
	cache map[string]tokenDecision
	// lastAllowed is when PMS last accepted each token.
	lastAllowed map[string]time.Time
}

func NewPlexTokenAuthorizer(upstream, ownerToken string, transport http.RoundTripper) (*PlexTokenAuthorizer, error) {
	u, err := url.Parse(strings.TrimSpace(upstream))
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("plexlabelproxy: upstream scheme must be http(s)")
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &PlexTokenAuthorizer{
		upstreamURL: u,
		ownerToken:  strings.TrimSpace(ownerToken),
		client:      &http.Client{Transport: transport, Timeout: 5 * time.Second},
		ttl:         5 * time.Minute,
		staleFor:    24 * time.Hour,
		retryAfter:  30 * time.Second,
		cache:       make(map[string]tokenDecision),
		lastAllowed: make(map[string]time.Time),
	}, nil
}

func (a *PlexTokenAuthorizer) AllowPlexToken(ctx context.Context, token string) bool {
	return a.AllowPlexTokenDetailed(ctx, token).Allowed
}

func (a *PlexTokenAuthorizer) AllowPlexTokenDetailed(ctx context.Context, token string) AuthorizationDecision {
	token = strings.TrimSpace(token)
	if token == "" {
		return AuthorizationDecision{}
	}
	if a.ownerToken != "" && token == a.ownerToken {
		return AuthorizationDecision{Allowed: true, CacheHit: true}
	}

	now := time.Now()
	a.mu.Lock()
	if d, ok := a.cache[token]; ok && now.Before(d.expires) {
		a.mu.Unlock()
		return AuthorizationDecision{Allowed: d.allowed, CacheHit: true}
	}
	a.mu.Unlock()

	allowed, answered := a.check(ctx, token)
	a.mu.Lock()
	defer a.mu.Unlock()
	if answered {
		a.cache[token] = tokenDecision{allowed: allowed, expires: now.Add(a.ttl)}
		if allowed {
			a.lastAllowed[token] = now
		} else {
			delete(a.lastAllowed, token)
		}
		return AuthorizationDecision{Allowed: allowed}
	}
	// PMS could not answer (restarting, busy, unreachable). A token it
	// accepted recently keeps working through the outage, rechecked every
	// retryAfter; any other token is refused without caching the refusal, so
	// the next request asks PMS again.
	if last, ok := a.lastAllowed[token]; ok && now.Sub(last) < a.staleFor {
		a.cache[token] = tokenDecision{allowed: true, expires: now.Add(a.retryAfter)}
		return AuthorizationDecision{Allowed: true, Unavailable: true}
	}
	return AuthorizationDecision{Unavailable: true}
}

// check asks PMS whether token has access to this server. answered is false
// when PMS gave no verdict: a transport error, a timeout, or a status other
// than 2xx, 401 or 403.
func (a *PlexTokenAuthorizer) check(ctx context.Context, token string) (allowed, answered bool) {
	u := *a.upstreamURL
	u.Path = "/library/sections"
	q := u.Query()
	q.Set("X-Plex-Token", token)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, false
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, true
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return false, true
	default:
		return false, false
	}
}
