package plexlabelproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A shared user's GET /media/providers is elevated like any Live TV request,
// so PMS answers with the owner's provider list: every library section on the
// server (shared with them or not), the owner's playlists and owner-level
// container attributes such as myPlexUsername. ownProviders answers it with
// the list PMS gives the caller's own token instead, plus the Live TV
// providers from the elevated answer, which is all they need the owner for.

// ownProvidersTimeout bounds the fetch with the caller's own token.
const ownProvidersTimeout = 5 * time.Second

// ownProvidersLogEvery is how often an own_list line is logged per token:
// clients ask for /media/providers a few times a minute.
const ownProvidersLogEvery = 5 * time.Minute

// ownProvidersLogMax bounds how many tokens' last own_list line is remembered.
const ownProvidersLogMax = 1000

// ownProviders fetches and logs a caller's own /media/providers.
type ownProviders struct {
	client  *http.Client
	timeout time.Duration

	mu     sync.Mutex
	logged map[string]time.Time
}

func newOwnProviders(transport http.RoundTripper) *ownProviders {
	return &ownProviders{
		client: &http.Client{
			Transport: transport,
			// A redirect is not the answer: never send the caller's token on.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		timeout: ownProvidersTimeout,
		logged:  make(map[string]time.Time),
	}
}

// ownProvidersCaller reports whether req is a GET /media/providers that
// borrowed the owner token for someone else under ElevateLiveTV, and that
// caller's own token ("" when they sent none and elevation recovered a
// session's). With ElevateAll the caller browses everything as the owner, so
// the owner's list is theirs too.
func (p *Proxy) ownProvidersCaller(req *http.Request) (string, bool) {
	if p.ownProviders == nil || !p.cfg.ElevateLiveTV || p.cfg.ElevateAll ||
		req == nil || req.URL == nil || req.Method != http.MethodGet ||
		req.URL.EscapedPath() != "/media/providers" {
		return "", false
	}
	token, elevated := req.Context().Value(clientTokenContextKey{}).(string)
	return token, elevated
}

// serveOwnProviders replaces an elevated /media/providers answer with the
// caller's own one plus the elevated Live TV providers. When that can't be
// done, the elevated answer is served as before, so Live TV keeps working.
// Runs in modifyResponse, before the allowTuners and label rewrites.
func (p *Proxy) serveOwnProviders(resp *http.Response) error {
	if resp == nil || resp.Request == nil || resp.Body == nil || resp.StatusCode != http.StatusOK {
		return nil
	}
	token, ok := p.ownProvidersCaller(resp.Request)
	if !ok {
		return nil
	}
	fp := tokenFingerprint(token)
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	isJSON := strings.Contains(ct, "json")
	switch {
	case !isJSON && !strings.Contains(ct, "xml"):
		p.ownProvidersFallback(fp, "unsupported_type", nil)
		return nil
	case token == "":
		// Never ask PMS without a token: it may answer a local request
		// without one as the owner.
		p.ownProvidersFallback(fp, "no_caller_token", nil)
		return nil
	case resp.ContentLength > maxScrubBodyBytes:
		p.ownProvidersFallback(fp, "too_large", nil)
		return nil
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.LimitReader(resp.Body, maxScrubBodyBytes+1), resp.Body}
	elevated, encoding, err := readBody(resp)
	if err != nil {
		return err
	}
	if len(elevated) > maxScrubBodyBytes {
		return fmt.Errorf("%s response is too large to replace with the caller's own", resp.Request.URL.EscapedPath())
	}

	own, reason, err := p.fetchOwnProviders(resp, token)
	if reason != "" {
		p.ownProvidersFallback(fp, reason, err)
		return restoreBody(resp, elevated, encoding)
	}
	out, providers, live, err := graftLiveProviders(own, elevated, isJSON)
	if err != nil {
		p.ownProvidersFallback(fp, "unparsed", err)
		return restoreBody(resp, elevated, encoding)
	}
	// The elevated answer's validators and lengths describe the owner's body.
	resp.Header.Del("ETag")
	resp.Header.Del("Last-Modified")
	resp.Header.Del("X-Plex-Content-Original-Length")
	resp.Header.Del("X-Plex-Content-Compressed-Length")
	resp.Header.Set("Cache-Control", "private, no-store")
	p.ownProvidersServed(fp, providers, live)
	return restoreBody(resp, out, encoding)
}

// fetchOwnProviders asks PMS for the elevated request again, with the
// caller's own token: same URL and query (less the token), same headers (so
// the same format), gzip at most. reason is "" on success, else why not.
func (p *Proxy) fetchOwnProviders(resp *http.Response, token string) (body []byte, reason string, err error) {
	ctx, cancel := context.WithTimeout(resp.Request.Context(), p.ownProviders.timeout)
	defer cancel()
	u := *resp.Request.URL
	q := u.Query()
	q.Del("X-Plex-Token")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "fetch_failed", err
	}
	req.Header = resp.Request.Header.Clone()
	req.Header.Del("If-None-Match")
	req.Header.Del("If-Modified-Since")
	req.Header.Del("X-Plex-User")
	req.Header.Set("X-Plex-Token", token)
	req.Header.Set("Accept-Encoding", "gzip")
	own, err := p.ownProviders.client.Do(req)
	if err != nil {
		return nil, fetchFailure(err), err
	}
	defer own.Body.Close()
	if own.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(own.Body, 64<<10))
		return nil, "status_" + strconv.Itoa(own.StatusCode), nil
	}
	if mediaType(own.Header.Get("Content-Type")) != mediaType(resp.Header.Get("Content-Type")) {
		return nil, "content_type_mismatch", nil
	}
	if own.ContentLength > maxScrubBodyBytes {
		return nil, "too_large", nil
	}
	own.Body = struct {
		io.Reader
		io.Closer
	}{io.LimitReader(own.Body, maxScrubBodyBytes+1), own.Body}
	body, _, err = readBody(own)
	if err != nil {
		return nil, fetchFailure(err), err
	}
	if len(body) > maxScrubBodyBytes {
		return nil, "too_large", nil
	}
	return body, "", nil
}

func fetchFailure(err error) string {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return "timeout"
	}
	return "fetch_failed"
}

// mediaType is a Content-Type without its parameters, lowercased.
func mediaType(contentType string) string {
	mt, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(mt))
}

func (p *Proxy) ownProvidersFallback(fp, reason string, err error) {
	// Not the request URL a client error names: only what went wrong.
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if err != nil {
		p.logger.Printf("plexlabelproxy_providers: outcome=fallback token_fp=%s reason=%s err=%q", fp, reason, err.Error())
		return
	}
	p.logger.Printf("plexlabelproxy_providers: outcome=fallback token_fp=%s reason=%s", fp, reason)
}

// ownProvidersServed logs an own_list line, at most once per token every
// ownProvidersLogEvery.
func (p *Proxy) ownProvidersServed(fp string, providers, live int) {
	o := p.ownProviders
	now := time.Now()
	o.mu.Lock()
	if last, ok := o.logged[fp]; ok && now.Sub(last) < ownProvidersLogEvery {
		o.mu.Unlock()
		return
	}
	if len(o.logged) >= ownProvidersLogMax {
		for k, last := range o.logged {
			if now.Sub(last) >= ownProvidersLogEvery {
				delete(o.logged, k)
			}
		}
		if len(o.logged) >= ownProvidersLogMax {
			clear(o.logged)
		}
	}
	o.logged[fp] = now
	o.mu.Unlock()
	p.logger.Printf("plexlabelproxy_providers: outcome=own_list token_fp=%s providers=%d live_tv=%d", fp, providers, live)
}

// graftLiveProviders puts the Live TV providers (liveProviderRE identifiers)
// of elevated, the owner's /media/providers answer, into own, the caller's:
// each replaces own's provider with the same identifier, or is appended,
// raising the container's size. Nothing else of own changes. It returns the
// result, how many providers it lists and how many Live TV providers came
// from elevated.
func graftLiveProviders(own, elevated []byte, isJSON bool) ([]byte, int, int, error) {
	if isJSON {
		return graftLiveProvidersJSON(own, elevated)
	}
	return graftLiveProvidersXML(own, elevated)
}

func graftLiveProvidersJSON(own, elevated []byte) ([]byte, int, int, error) {
	ownDoc, ownMC, ownList, err := decodeProvidersJSON(own)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("own answer: %w", err)
	}
	_, _, elevatedList, err := decodeProvidersJSON(elevated)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("elevated answer: %w", err)
	}
	var live []map[string]any
	for _, el := range elevatedList {
		if mp, ok := el.(map[string]any); ok && liveProviderRE.MatchString(jsonString(mp["identifier"])) {
			live = append(live, mp)
		}
	}
	if len(live) == 0 {
		return own, len(ownList), 0, nil
	}
	added := 0
	for _, mp := range live {
		id := jsonString(mp["identifier"])
		replaced := false
		for i, el := range ownList {
			if cur, ok := el.(map[string]any); ok && jsonString(cur["identifier"]) == id {
				ownList[i] = mp
				replaced = true
				break
			}
		}
		if !replaced {
			ownList = append(ownList, mp)
			added++
		}
	}
	ownMC["MediaProvider"] = ownList
	if n, err := strconv.Atoi(jsonString(ownMC["size"])); err == nil && added > 0 {
		ownMC["size"] = json.Number(strconv.Itoa(n + added))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ownDoc); err != nil {
		return nil, 0, 0, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), len(ownList), len(live), nil
}

// decodeProvidersJSON decodes a /media/providers JSON answer: the document,
// its MediaContainer and that container's MediaProvider list.
func decodeProvidersJSON(body []byte) (map[string]any, map[string]any, []any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, nil, err
	}
	mc, ok := doc["MediaContainer"].(map[string]any)
	if !ok {
		return nil, nil, nil, errors.New("no MediaContainer")
	}
	list, ok := mc["MediaProvider"].([]any)
	if !ok && mc["MediaProvider"] != nil {
		return nil, nil, nil, errors.New("MediaProvider is not a list")
	}
	return doc, mc, list, nil
}

func graftLiveProvidersXML(own, elevated []byte) ([]byte, int, int, error) {
	declHead, ownDoc, err := parseXMLTree(own)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("own answer: %w", err)
	}
	ownMC := providersContainerXML(ownDoc)
	if ownMC == nil {
		return nil, 0, 0, errors.New("own answer: no MediaContainer")
	}
	_, elevatedDoc, err := parseXMLTree(elevated)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("elevated answer: %w", err)
	}
	elevatedMC := providersContainerXML(elevatedDoc)
	if elevatedMC == nil {
		return nil, 0, 0, errors.New("elevated answer: no MediaContainer")
	}
	var live []*xmlNode
	for _, ch := range elevatedMC.children {
		if e, ok := ch.(*xmlNode); ok && e.start.Name.Local == "MediaProvider" && liveProviderRE.MatchString(e.attr("identifier")) {
			live = append(live, e)
		}
	}
	if len(live) == 0 {
		return own, ownMC.elementCount("MediaProvider"), 0, nil
	}
	added := 0
	for _, mp := range live {
		id := mp.attr("identifier")
		replaced := false
		for i, ch := range ownMC.children {
			if e, ok := ch.(*xmlNode); ok && e.start.Name.Local == "MediaProvider" && e.attr("identifier") == id {
				ownMC.children[i] = mp
				replaced = true
				break
			}
		}
		if !replaced {
			ownMC.children = append(ownMC.children, mp, xml.CharData("\n"))
			added++
		}
	}
	if n, err := strconv.Atoi(ownMC.attr("size")); err == nil && added > 0 {
		ownMC.setAttr("size", strconv.Itoa(n+added))
	}
	out, err := encodeXMLTree(declHead, ownDoc)
	if err != nil {
		return nil, 0, 0, err
	}
	return out, ownMC.elementCount("MediaProvider"), len(live), nil
}

// providersContainerXML is doc's root element if it's a MediaContainer.
func providersContainerXML(doc *xmlNode) *xmlNode {
	for _, ch := range doc.children {
		if e, ok := ch.(*xmlNode); ok {
			if e.start.Name.Local == "MediaContainer" {
				return e
			}
			return nil
		}
	}
	return nil
}
