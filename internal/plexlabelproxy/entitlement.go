package plexlabelproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// ApplyLiveTVTokenElevation rewrites a Plex request to use ownerToken only
// when the request targets Plex Live TV surfaces. It returns true when a
// rewrite was applied.
//
// This intentionally does not elevate generic library, playback, metadata, or
// account paths. The deployment model is "users browse Plex as themselves;
// Live TV calls borrow the PMS owner's tuner entitlement."
func ApplyLiveTVTokenElevation(req *http.Request, ownerToken string) bool {
	token := strings.TrimSpace(ownerToken)
	if token == "" || !IsLiveTVRequest(req) {
		return false
	}
	q := req.URL.Query()
	q.Set("X-Plex-Token", token)
	req.URL.RawQuery = q.Encode()
	req.Header.Set("X-Plex-Token", token)
	return true
}

// ApplyLiveTVDiscoveryElevation elevates only Live TV browse and metadata
// requests to ownerToken, intentionally excluding stream-start paths
// (/video/:/transcode/ and /playQueues). Stream requests use the client's own
// token so any resulting Plex session is attributed to the user, not the owner.
// Returns true when a rewrite was applied.
func ApplyLiveTVDiscoveryElevation(req *http.Request, ownerToken string) bool {
	token := strings.TrimSpace(ownerToken)
	if token == "" || !IsLiveTVDiscoveryRequest(req) {
		return false
	}
	q := req.URL.Query()
	q.Set("X-Plex-Token", token)
	req.URL.RawQuery = q.Encode()
	req.Header.Set("X-Plex-Token", token)
	return true
}

// IsLiveTVDiscoveryRequest classifies Live TV browse and metadata requests
// only. Unlike IsLiveTVRequest it intentionally excludes /video/:/transcode/
// and /playQueues so that actual stream sessions are attributed to the client's
// own token rather than the owner's. Use with ApplyLiveTVDiscoveryElevation
// to test whether Plex enforces entitlement at stream time or only at setup.
func IsLiveTVDiscoveryRequest(req *http.Request) bool {
	if req == nil || req.URL == nil || !canonicalRequestPath(req.URL.EscapedPath()) {
		return false
	}
	if !liveTVElevationMethod(req.Method) {
		return false
	}
	path := req.URL.EscapedPath()
	switch {
	case path == "/media/providers":
		return true
	case isLiveTVSubscriptionRequest(req):
		return true
	case path == "/media/grabbers/devices":
		return true
	case strings.HasPrefix(path, "/livetv/"):
		return true
	case strings.HasPrefix(path, "/tv.plex.providers.epg.xmltv:"):
		return true
	}
	if path == "/" || path == "/identity" {
		return refererIsLiveTV(req.Header.Get("Referer"))
	}
	// Intentionally excludes /video/:/transcode/ and /playQueues.
	return false
}

// IsLiveTVStreamRequest returns true when the request creates a Plex playback
// session for Live TV content. These are the paths that cause Plex to attribute
// a session — and therefore watch history — to whichever token is used.
func IsLiveTVStreamRequest(req *http.Request) bool {
	if req == nil || req.URL == nil || !canonicalRequestPath(req.URL.EscapedPath()) {
		return false
	}
	path := req.URL.EscapedPath()
	switch {
	case strings.HasPrefix(path, "/livetv/dvrs/") && strings.Contains(path, "/channels/") && strings.HasSuffix(path, "/tune"):
		return true
	case strings.HasPrefix(path, "/video/:/transcode/"):
		return queryParamIsLiveTVPath(req.URL.Query(), "path")
	case strings.HasPrefix(path, "/playQueues"):
		return queryParamIsLiveTVPath(req.URL.Query(), "uri") ||
			queryParamIsLiveTVPath(req.URL.Query(), "path") ||
			bodyParamIsLiveTVPath(req, "uri") ||
			bodyParamIsLiveTVPath(req, "path")
	}
	return false
}

// IsLiveTVRequest classifies PMS requests whose authorization needs Plex Live
// TV tuner entitlement rather than ordinary library share access.
//
// Intentionally broad: Plex clients send Live TV path/uri values in query
// parameters and Referer headers across many request types. Narrow matching
// misses stream-start and session-setup calls that Plex validates against the
// tuner entitlement.
func IsLiveTVRequest(req *http.Request) bool {
	if req == nil || req.URL == nil || !canonicalRequestPath(req.URL.EscapedPath()) {
		return false
	}
	if !liveTVElevationMethod(req.Method) {
		if isLiveTVSubscriptionRequest(req) {
			return true
		}
		if strings.EqualFold(req.Method, http.MethodPost) && IsLiveTVStreamRequest(req) {
			return true
		}
		return false
	}
	path := req.URL.EscapedPath()
	switch {
	case path == "/media/providers":
		return true
	case isLiveTVSubscriptionRequest(req):
		return true
	case path == "/media/grabbers/devices":
		return true
	case strings.HasPrefix(path, "/media/grabbers/"):
		return true
	case strings.HasPrefix(path, "/livetv/"):
		return true
	case strings.HasPrefix(path, "/tv.plex.providers.epg.xmltv:"):
		return true
	}
	if IsLiveTVStreamRequest(req) {
		return true
	}
	if path == "/" || path == "/identity" {
		return refererIsLiveTV(req.Header.Get("Referer"))
	}
	return false
}

func isLiveTVSubscriptionRequest(req *http.Request) bool {
	if req == nil || req.URL == nil || !canonicalRequestPath(req.URL.EscapedPath()) {
		return false
	}
	path := req.URL.EscapedPath()
	if !isMediaSubscriptionPath(path) {
		return false
	}
	if !mediaSubscriptionMethod(req.Method) {
		return false
	}
	if isReadMethod(req.Method) {
		if path == "/media/subscriptions/template" {
			return subscriptionRequestHasLiveTVEvidence(req)
		}
		return true
	}
	if subscriptionDeleteHasPlaybackSessionEvidence(req) {
		return true
	}
	return subscriptionRequestHasLiveTVEvidence(req)
}

func subscriptionDeleteHasPlaybackSessionEvidence(req *http.Request) bool {
	if req == nil || req.URL == nil || !strings.EqualFold(req.Method, http.MethodDelete) {
		return false
	}
	if !isSingleMediaSubscriptionIDPath(req.URL.EscapedPath()) {
		return false
	}
	return requestValueNonEmpty(req, "X-Plex-Playback-Session-Id") ||
		requestValueNonEmpty(req, "X-Plex-Session-Id")
}

func isSingleMediaSubscriptionIDPath(path string) bool {
	const prefix = "/media/subscriptions/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	id := strings.TrimPrefix(path, prefix)
	if id == "" || strings.Contains(id, "/") {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func requestValueNonEmpty(req *http.Request, name string) bool {
	for key, values := range req.URL.Query() {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, v := range values {
			if strings.TrimSpace(v) != "" {
				return true
			}
		}
	}
	for _, v := range req.Header.Values(name) {
		if strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

func subscriptionRequestHasLiveTVEvidence(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	q := req.URL.Query()
	if queryParamIsLiveTVPath(q, "guid") || queryParamIsLiveTVPath(q, "key") || queryParamIsLiveTVPath(q, "uri") {
		return true
	}
	if queryValuesContainLiveTVPath(q) {
		return true
	}
	if methodMayHaveBody(req.Method) {
		return bodyContainsLiveTVPath(req)
	}
	return false
}

func isMediaSubscriptionPath(path string) bool {
	return path == "/media/subscriptions" ||
		path == "/media/subscriptions/template" ||
		path == "/media/subscriptions/scheduled" ||
		strings.HasPrefix(path, "/media/subscriptions/")
}

func mediaSubscriptionMethod(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "", http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func isReadMethod(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "", http.MethodGet, http.MethodHead:
		return true
	default:
		return false
	}
}

func methodMayHaveBody(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func liveTVElevationMethod(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "", http.MethodGet, http.MethodHead:
		return true
	default:
		return false
	}
}

func queryParamIsLiveTVPath(q url.Values, name string) bool {
	for _, v := range q[name] {
		if liveTVTarget(v) {
			return true
		}
	}
	return false
}

func queryValuesContainLiveTVPath(q url.Values) bool {
	for key, values := range q {
		if !subscriptionHintKey(key) {
			continue
		}
		for _, v := range values {
			if liveTVTarget(v) {
				return true
			}
		}
	}
	return false
}

func subscriptionHintKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	switch key {
	case "guid", "key", "uri", "ratingkey":
		return true
	}
	return strings.HasSuffix(key, "[guid]") ||
		strings.HasSuffix(key, "[key]") ||
		strings.HasSuffix(key, "[uri]") ||
		strings.HasSuffix(key, "[ratingkey]")
}

func bodyParamIsLiveTVPath(req *http.Request, name string) bool {
	body, ok := readRequestBodyForClassification(req)
	if !ok {
		return false
	}
	return bodyBytesContainLiveTVPath(req, body, name)
}

func bodyContainsLiveTVPath(req *http.Request) bool {
	body, ok := readRequestBodyForClassification(req)
	if !ok {
		return false
	}
	ct := strings.ToLower(req.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/x-www-form-urlencoded") || ct == "" {
		if values, err := url.ParseQuery(string(body)); err == nil {
			if queryParamIsLiveTVPath(values, "guid") ||
				queryParamIsLiveTVPath(values, "key") ||
				queryParamIsLiveTVPath(values, "uri") ||
				queryParamIsLiveTVPath(values, "ratingKey") ||
				queryValuesContainLiveTVPath(values) {
				return true
			}
		}
	}
	return jsonBodyHasLiveTVTarget(body, subscriptionHintKey)
}

func readRequestBodyForClassification(req *http.Request) ([]byte, bool) {
	if req == nil || req.Body == nil {
		return nil, false
	}
	const maxBody = 1 << 20
	body, err := io.ReadAll(io.LimitReader(req.Body, maxBody+1))
	if err != nil {
		return nil, false
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	if req.ContentLength >= 0 && int64(len(body)) <= maxBody {
		req.ContentLength = int64(len(body))
	}
	if len(body) > maxBody {
		return nil, false
	}
	return body, true
}

func bodyBytesContainLiveTVPath(req *http.Request, body []byte, name string) bool {
	ct := strings.ToLower(req.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/x-www-form-urlencoded") || ct == "" {
		if values, err := url.ParseQuery(string(body)); err == nil && queryParamIsLiveTVPath(values, name) {
			return true
		}
	}
	return jsonBodyHasLiveTVTarget(body, func(key string) bool { return strings.EqualFold(key, name) })
}

// canonicalRequestPath reports whether an escaped request path is in the form
// Plex serves it as-is: absolute, with no "." or ".." segments, no empty
// segments, and no escaped dots, slashes or backslashes. Elevation is decided
// on the path as written, so a path Plex would resolve to something else
// (/livetv/../:/prefs, /livetv/%2e%2e/library/sections) must never be elevated.
func canonicalRequestPath(escaped string) bool {
	if escaped == "" || escaped[0] != '/' {
		return false
	}
	lower := strings.ToLower(escaped)
	for _, bad := range []string{"%2e", "%2f", "%5c", "\\"} {
		if strings.Contains(lower, bad) {
			return false
		}
	}
	// One trailing slash is allowed (/livetv/dvrs/)
	trimmed := strings.TrimSuffix(escaped[1:], "/")
	if trimmed == "" {
		return true
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// liveTVTarget reports whether a request parameter value names a Live TV
// resource: an XMLTV guid, an XMLTV EPG provider key, or a path under /livetv/
// or an XMLTV EPG provider, directly or as the path of a server:// URI. Values
// are decoded (Plex clients nest escaping) and paths cleaned. The query string
// and fragment never count, so a library path that only mentions Live TV
// (/library/metadata/1?x=/livetv/) or climbs out of it
// (/livetv/../library/metadata/1) is not a Live TV target.
func liveTVTarget(v string) bool {
	for i := 0; i < 4; i++ {
		if liveTVTargetRaw(v) {
			return true
		}
		next, err := url.QueryUnescape(v)
		if err != nil || next == v {
			next, err = url.PathUnescape(v)
			if err != nil || next == v {
				return false
			}
		}
		v = next
	}
	return liveTVTargetRaw(v)
}

func liveTVTargetRaw(v string) bool {
	v = strings.TrimSpace(v)
	lower := strings.ToLower(v)
	// XMLTV guids identify guide items, not server paths
	if strings.HasPrefix(lower, "tv.plex.xmltv:") {
		return true
	}
	// A provider-relative key such as tv.plex.providers.epg.xmltv:123/grid
	// parses as an opaque URI, so treat it as the path it is
	if strings.HasPrefix(lower, "tv.plex.providers.epg.xmltv:") {
		if i := strings.IndexAny(v, "?#"); i >= 0 {
			v = v[:i]
		}
		return liveTVPath("/" + v)
	}
	u, err := url.Parse(v)
	if err != nil {
		return false
	}
	if liveTVPath(u.Path) {
		return true
	}
	// server://<machine>/<provider>/<path>: the first segment names the
	// provider, the rest is the path within it
	if u.Scheme != "" && u.Host != "" {
		rest := strings.TrimPrefix(u.Path, "/")
		if i := strings.Index(rest, "/"); i >= 0 {
			return liveTVPath(rest[i:])
		}
	}
	return false
}

// liveTVPath reports whether p, once cleaned, is under /livetv/ or an XMLTV
// EPG provider.
func liveTVPath(p string) bool {
	if p == "" {
		return false
	}
	clean := strings.ToLower(path.Clean(p))
	return strings.HasPrefix(clean, "/livetv/") ||
		strings.HasPrefix(clean, "/tv.plex.providers.epg.xmltv:")
}

// jsonBodyHasLiveTVTarget reports whether a JSON body has a string value,
// under a key accepted by keyMatches, that names a Live TV target. Other
// keys and non-JSON bodies don't count: text that merely contains a Live TV
// path somewhere in the body is not evidence.
func jsonBodyHasLiveTVTarget(body []byte, keyMatches func(string) bool) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return false
	}
	var doc any
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		return false
	}
	var walk func(any) bool
	walk = func(node any) bool {
		switch n := node.(type) {
		case map[string]any:
			for key, value := range n {
				if s, ok := value.(string); ok && keyMatches(key) && liveTVTarget(s) {
					return true
				}
				if walk(value) {
					return true
				}
			}
		case []any:
			for _, value := range n {
				if walk(value) {
					return true
				}
			}
		}
		return false
	}
	return walk(doc)
}

func refererIsLiveTV(ref string) bool {
	return liveTVText(ref)
}

func liveTVText(s string) bool {
	for i := 0; i < 4; i++ {
		if rawLiveTVText(s) {
			return true
		}
		unescaped, err := url.QueryUnescape(s)
		if err != nil || unescaped == s {
			unescaped, err = url.PathUnescape(s)
			if err != nil || unescaped == s {
				break
			}
		}
		s = unescaped
	}
	return rawLiveTVText(s)
}

func rawLiveTVText(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "/livetv/") ||
		strings.Contains(s, "tv.plex.xmltv:") ||
		strings.Contains(s, "tv.plex.providers.epg.xmltv:") ||
		strings.Contains(s, "livetv%2f") ||
		strings.Contains(s, "livetv%252f") ||
		strings.Contains(s, "tv.plex.xmltv%3a") ||
		strings.Contains(s, "tv%2eplex%2exmltv%3a") ||
		strings.Contains(s, "tv.plex.xmltv%253a") ||
		strings.Contains(s, "tv%252eplex%252exmltv%253a") ||
		strings.Contains(s, "tv.plex.providers.epg.xmltv%3a") ||
		strings.Contains(s, "tv%2eplex%2eproviders%2eepg%2exmltv%3a") ||
		strings.Contains(s, "tv.plex.providers.epg.xmltv%253a") ||
		strings.Contains(s, "tv%252eplex%252eproviders%252eepg%252exmltv%253a")
}

// RewriteTunerEntitlementFlags rewrites the small XML/JSON hints Plex Web uses to
// decide whether the account can see Live TV entry points. It is deliberately
// narrow: it only changes allowTuners fields and never rewrites account,
// library, or server identity.
func RewriteTunerEntitlementFlags(body []byte) []byte {
	if len(body) == 0 || !bytes.Contains(body, []byte("allowTuners")) {
		return body
	}
	out := bytes.ReplaceAll(body, []byte(`allowTuners="0"`), []byte(`allowTuners="1"`))
	out = bytes.ReplaceAll(out, []byte(`<Setting id="allowTuners" value="0"`), []byte(`<Setting id="allowTuners" value="1"`))
	out = bytes.ReplaceAll(out, []byte(`"allowTuners":false`), []byte(`"allowTuners":true`))
	out = bytes.ReplaceAll(out, []byte(`"allowTuners": false`), []byte(`"allowTuners": true`))
	out = bytes.ReplaceAll(out, []byte(`"allowTuners":0`), []byte(`"allowTuners":1`))
	out = bytes.ReplaceAll(out, []byte(`"allowTuners": 0`), []byte(`"allowTuners": 1`))
	return out
}
