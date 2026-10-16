package plexlabelproxy

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// -hide-live-channel: hide some Live TV channels (a test channel, say) from
// the people the server is shared with, while the owner still sees and tunes
// them.
//
// Plex has no per-user channel list: everyone with Live TV sees the DVR's
// whole lineup. Shared users only get Live TV through this proxy, so the proxy
// drops the hidden channels from the Live TV responses it hands them (channel
// lists, guide grids, hubs, search, DVR mappings) and refuses to tune them.
//
// Who's the owner: the owner's apps each sign in with their own token, not
// the owner token the proxy holds, so the share list decides. A token on
// plex.tv's share list for this server is a shared user's (and stays one); a
// token missing from a share list fetched after it was first seen is one of
// the owner's devices; anything not settled yet is treated like a shared user
// (the channel stays hidden).

// ViewAsHeader, set to "friend" on a request carrying the owner token, shows
// the response the way a shared user would get it, so the hiding can be
// checked without a shared user's token. It's never passed on to PMS.
const ViewAsHeader = "X-Iptvtunerr-View-As"

const (
	// hiddenMapMaxAge is how often the DVR channel map is refetched (at most).
	hiddenMapMaxAge = time.Minute
	// classifyWait is how long a response waits for a share-list fetch when
	// its caller isn't settled yet.
	classifyWait = 2 * time.Second
)

// liveTVChannelPathRE matches /livetv/dvrs/<dvr>/channels/<channel>[/...],
// which includes the tune request, as a request's own path or as a target
// nested in one (a transcode's path, a play queue's uri).
var liveTVChannelPathRE = regexp.MustCompile(`^/livetv/dvrs/[^/]+/channels/([^/]+)(?:/|$)`)

// hiddenChannels is what's hidden. A channel is named by its number (the
// guide's vcn, the channel mapping's lineup and device identifiers), which
// hides that number on every DVR, or by its identifier (the XMLTV channel id,
// e.g. C99.114.tunarr.com), which hides that channel only. Nothing is learned
// from responses: a number's identifiers come from Tunarr's pattern and the
// current DVR channel map.
type hiddenChannels struct {
	configured []string
	fetchMap   func(ctx context.Context) ([]channelMapping, error)
	// numbers and identValues are the configured numbers and identifiers;
	// tunarrIdents Tunarr's identifiers for the numbers. None change.
	numbers, identValues, tunarrIdents map[string]bool

	mu sync.Mutex
	// mapIdents are the identifiers the last DVR channel map gives the hidden
	// numbers, and tunerChannels "<device>\x00<tuner channel>" for the
	// configured identifiers (the tuner's channel list names channels by
	// number only, and the same number may be another channel on another
	// device). Each map replaces both, never changing them in place.
	mapIdents, tunerChannels map[string]bool
	// unpaired holds configured identifiers the DVR channel map hasn't paired
	// with a tuner channel; while there are any, the map is refetched (at most
	// every hiddenMapMaxAge) before filtering. It's also refetched before a
	// tuner's channel list is filtered.
	unpaired    map[string]bool
	mapTriedAt  time.Time
	mapResolved bool
}

type channelMapping struct {
	device, key, deviceID, lineupID string
}

// parseHiddenChannelValues splits the -hide-live-channel values (repeated or
// comma-separated) and drops empties and duplicates.
func parseHiddenChannelValues(values []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part != "" && !seen[part] {
				seen[part] = true
				out = append(out, part)
			}
		}
	}
	return out
}

func newHiddenChannels(values []string, fetchMap func(ctx context.Context) ([]channelMapping, error)) *hiddenChannels {
	h := &hiddenChannels{
		configured:    values,
		fetchMap:      fetchMap,
		numbers:       map[string]bool{},
		identValues:   map[string]bool{},
		tunarrIdents:  map[string]bool{},
		mapIdents:     map[string]bool{},
		tunerChannels: map[string]bool{},
		unpaired:      map[string]bool{},
	}
	for _, v := range values {
		if isChannelNumber(v) {
			h.numbers[v] = true
			// Tunarr's XMLTV id for a channel number, which is what Plex keys
			// the channel by: known before the channel map lists it.
			addNonEmpty(h.tunarrIdents, tunarrChannelID(v))
		} else {
			h.identValues[v] = true
			h.unpaired[v] = true
		}
	}
	return h
}

func isChannelNumber(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}

// tunarrChannelID is the XMLTV channel id Tunarr gives a channel number
// (getChannelId): "C<number>.<sum of each digit plus 48>.tunarr.com", e.g.
// C99.114.tunarr.com. "" for anything but a whole number, which Tunarr
// doesn't use.
func tunarrChannelID(number string) string {
	if n, err := strconv.Atoi(number); err != nil || n <= 0 || strconv.Itoa(n) != number {
		return ""
	}
	sum := 0
	for _, r := range number {
		sum += int(r) // a digit plus 48 is its character code
	}
	return fmt.Sprintf("C%s.%d.tunarr.com", number, sum)
}

// needsMap reports whether a configured identifier still lacks its tuner
// channel, so the DVR channel map should be (re)read before filtering.
func (h *hiddenChannels) needsMap() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.unpaired) > 0
}

func addNonEmpty(m map[string]bool, values ...string) {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			m[v] = true
		}
	}
}

// refreshMap refetches the DVR channel map when it's older than
// hiddenMapMaxAge: a hidden number's identifiers on every DVR, and a hidden
// identifier's tuner channel on its device.
func (h *hiddenChannels) refreshMap(ctx context.Context) {
	h.mu.Lock()
	if h.fetchMap == nil || (!h.mapTriedAt.IsZero() && time.Since(h.mapTriedAt) < hiddenMapMaxAge) {
		h.mu.Unlock()
		return
	}
	h.mapTriedAt = time.Now()
	h.mu.Unlock()
	mappings, err := h.fetchMap(ctx)
	if err != nil {
		return
	}
	// Rebuilt from each map, as Plex may have moved a channel.
	idents, tuner, paired, resolved := map[string]bool{}, map[string]bool{}, map[string]bool{}, false
	for _, m := range mappings {
		switch {
		case h.numbers[m.lineupID] || h.numbers[m.deviceID]:
			addNonEmpty(idents, m.key)
			resolved = true
		case h.identValues[m.key]:
			if m.deviceID != "" {
				tuner[m.device+"\x00"+m.deviceID] = true
				paired[m.key] = true
			}
			resolved = true
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mapIdents, h.tunerChannels, h.mapResolved = idents, tuner, resolved
	for v := range h.identValues {
		if paired[v] {
			delete(h.unpaired, v)
		} else {
			h.unpaired[v] = true
		}
	}
}

// hiddenSet is what's hidden when a request is judged.
type hiddenSet struct {
	numbers, idents, tunerChannels map[string]bool
}

func (h *hiddenChannels) snapshot() hiddenSet {
	h.mu.Lock()
	defer h.mu.Unlock()
	idents := make(map[string]bool, len(h.identValues)+len(h.tunarrIdents)+len(h.mapIdents))
	for _, m := range []map[string]bool{h.identValues, h.tunarrIdents, h.mapIdents} {
		for id := range m {
			idents[id] = true
		}
	}
	return hiddenSet{numbers: h.numbers, idents: idents, tunerChannels: h.tunerChannels}
}

// describe lists the hidden channels for the startup log.
func (h *hiddenChannels) describe() string {
	set := h.snapshot()
	idents := make([]string, 0, len(set.idents))
	for id := range set.idents {
		idents = append(idents, id)
	}
	sort.Strings(idents)
	h.mu.Lock()
	state := "not in Plex's channel map yet"
	if h.mapResolved {
		state = "in Plex's channel map"
	}
	h.mu.Unlock()
	return fmt.Sprintf("%s (%s; %s)", strings.Join(h.configured, ", "), strings.Join(idents, ", "), state)
}

// channelHidden reports whether a channel named in a request path is hidden.
func (h *hiddenChannels) channelHidden(id string) bool {
	set := h.snapshot()
	return set.idents[id] || set.numbers[id]
}

// entryHidden reports whether one entry (a JSON object or XML element named
// name, with fields read by get) belongs to a hidden channel. Only the fields
// that name a channel are compared, never titles of programs. A hidden
// airing or lineup entry tells this response's Channel tags which Plex
// channel ids and titles to drop.
func (r *hideRun) entryHidden(name string, get func(string) string) bool {
	s := r.set
	switch name {
	case "Media":
		// An airing in the guide: the channel it airs on.
		if s.idents[get("channelIdentifier")] || s.numbers[get("channelVcn")] {
			addNonEmpty(r.plexIDs, get("channelID"))
			addNonEmpty(r.titles, get("channelTitle"))
			return true
		}
	case "ChannelMapping":
		// /livetv/dvrs: the DVR's mapping of tuner channels to guide channels.
		return s.idents[get("channelKey")] || s.numbers[get("lineupIdentifier")] || s.numbers[get("deviceIdentifier")]
	case "DeviceChannel":
		// /media/grabbers/devices/<device>/channels: the tuner's channels.
		id := get("identifier")
		return s.numbers[id] || (r.device != "" && s.tunerChannels[r.device+"\x00"+id])
	case "Channel":
		if get("filter") != "" || (get("tag") != "" && get("vcn") == "" && get("gridKey") == "") {
			// A Channel tag on a program: id is Plex's internal channel id,
			// tag the channel title, as the program's airings named them.
			if id := get("id"); id != "" {
				return r.plexIDs[id]
			}
			return r.titles[get("tag")]
		}
		// A lineup channel (lineups/dvr/channels, epg lineupchannels).
		if s.idents[get("id")] || s.idents[get("gridKey")] || s.idents[get("key")] || s.idents[get("identifier")] ||
			s.numbers[get("vcn")] || s.numbers[get("channelVcn")] || s.numbers[get("identifier")] {
			if vcn := firstNonEmpty(get("vcn"), get("channelVcn")); vcn != "" {
				if cs := get("callSign"); cs != "" {
					r.titles[vcn+" "+cs] = true
				}
			}
			return true
		}
	}
	return false
}

// leakIn reports a hidden channel's identifier, or a quoted title of one this
// response named, found in a filtered body, for the leak_suspect log line: a
// response shape the filter doesn't know.
func (r *hideRun) leakIn(body []byte) string {
	for id := range r.set.idents {
		if bytes.Contains(body, []byte(id)) {
			return id
		}
	}
	for title := range r.titles {
		if bytes.Contains(body, []byte(`"`+title+`"`)) {
			return title
		}
	}
	return ""
}

// hideRun filters one response body.
type hideRun struct {
	set hiddenSet
	// device is the tuner device whose channels the response lists, if any.
	device string
	// plexIDs and titles are the Plex channel ids and "<vcn> <callSign>"
	// titles of the hidden channels this response named, for its Channel
	// tags and the leak check.
	plexIDs, titles map[string]bool
	// removed counts every entry removed; rootRemoved the root container's
	// direct children removed (for X-Plex-Container-Total-Size).
	removed, rootRemoved int
	// present counts hidden entries without removing them (owner debug).
	countOnly bool
	present   int
}

func (h *hiddenChannels) newRun(device string, countOnly bool) *hideRun {
	return &hideRun{set: h.snapshot(), device: device, countOnly: countOnly, plexIDs: map[string]bool{}, titles: map[string]bool{}}
}

// itemFirst orders an object's keys so airings (Media) are judged before the
// Channel tags that name the same channel by title.
func itemFirstKeys(obj map[string]any) []string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if (keys[i] == "Media") != (keys[j] == "Media") {
			return keys[i] == "Media"
		}
		return keys[i] < keys[j]
	})
	return keys
}

func jsonString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	}
	return ""
}

// filterJSON removes hidden channels' entries from a Plex JSON response.
// The body is re-encoded only when something was removed.
func (r *hideRun) filterJSON(body []byte) ([]byte, bool, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, false, err
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return body, false, nil
	}
	mc, ok := root["MediaContainer"].(map[string]any)
	if !ok {
		return body, false, nil
	}
	r.rootRemoved = r.filterChildrenJSON(mc)
	if r.countOnly || r.removed == 0 {
		return body, false, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, false, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), true, nil
}

// filterChildrenJSON filters obj's child entries and returns how many of its
// direct children were removed, lowering its size and totalSize to match.
func (r *hideRun) filterChildrenJSON(obj map[string]any) int {
	removed := 0
	for _, k := range itemFirstKeys(obj) {
		switch v := obj[k].(type) {
		case []any:
			kept := make([]any, 0, len(v))
			for _, el := range v {
				child, ok := el.(map[string]any)
				if ok && r.dropJSON(k, child) {
					removed++
					continue
				}
				kept = append(kept, el)
			}
			if len(kept) != len(v) && !r.countOnly {
				if len(kept) == 0 {
					delete(obj, k)
				} else {
					obj[k] = kept
				}
			}
		case map[string]any:
			if r.dropJSON(k, v) {
				removed++
				if !r.countOnly {
					delete(obj, k)
				}
			}
		}
	}
	if removed > 0 && !r.countOnly {
		for _, sizeKey := range []string{"size", "totalSize"} {
			if n, err := strconv.Atoi(jsonString(obj[sizeKey])); err == nil {
				obj[sizeKey] = json.Number(strconv.Itoa(max(0, n-removed)))
			}
		}
	}
	return removed
}

// dropJSON filters el's own children, then reports whether el itself goes: it
// is a hidden channel's entry, or a program left with no airings, or a search
// result left without its program.
func (r *hideRun) dropJSON(name string, el map[string]any) bool {
	if r.entryHidden(name, func(k string) string { return jsonString(el[k]) }) {
		r.count()
		return true
	}
	media, _ := el["Media"].([]any)
	hadMedia := len(media) > 0
	_, hadItem := el["Metadata"].(map[string]any)
	before := r.removed
	r.filterChildrenJSON(el)
	// Counting only (owner debug), the hidden entries themselves are counted
	// and nothing is removed, so a parent never empties.
	if r.countOnly || r.removed == before {
		return false
	}
	if hadMedia {
		if m, _ := el["Media"].([]any); len(m) == 0 {
			r.count()
			return true
		}
	}
	if name == "SearchResult" && hadItem {
		if _, still := el["Metadata"]; !still {
			r.count()
			return true
		}
	}
	return false
}

func (r *hideRun) count() {
	if r.countOnly {
		r.present++
	} else {
		r.removed++
	}
}

// xmlNode is an element with its children: *xmlNode or other xml tokens.
type xmlNode struct {
	start    xml.StartElement
	children []any
}

func (n *xmlNode) attr(name string) string { return attrValue(n.start.Attr, name) }

func (n *xmlNode) setAttr(name, value string) {
	for i := range n.start.Attr {
		if n.start.Attr[i].Name.Local == name {
			n.start.Attr[i].Value = value
			return
		}
	}
}

func (n *xmlNode) elementCount(name string) int {
	c := 0
	for _, ch := range n.children {
		if e, ok := ch.(*xmlNode); ok && (name == "" || e.start.Name.Local == name) {
			c++
		}
	}
	return c
}

// filterXML removes hidden channels' entries from a Plex XML response. The
// XML declaration is kept as it was, like rewriteTokens; the body is
// re-encoded only when something was removed.
func (r *hideRun) filterXML(body []byte) ([]byte, bool, error) {
	declHead := []byte{}
	rest := body
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if bytes.HasPrefix(trimmed, []byte("<?xml")) {
		if end := bytes.Index(trimmed, []byte("?>")); end > 0 {
			declHead = append(declHead, trimmed[:end+2]...)
			rest = trimmed[end+2:]
		}
	}
	dec := xml.NewDecoder(bytes.NewReader(rest))
	dec.Strict = false
	doc := &xmlNode{}
	stack := []*xmlNode{doc}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false, err
		}
		top := stack[len(stack)-1]
		switch t := tok.(type) {
		case xml.ProcInst:
			continue
		case xml.StartElement:
			n := &xmlNode{start: t.Copy()}
			top.children = append(top.children, n)
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		default:
			top.children = append(top.children, xml.CopyToken(tok))
		}
	}
	for _, ch := range doc.children {
		if root, ok := ch.(*xmlNode); ok {
			r.rootRemoved += r.filterChildrenXML(root)
		}
	}
	if r.countOnly || r.removed == 0 {
		return body, false, nil
	}
	var buf bytes.Buffer
	buf.Write(declHead)
	enc := xml.NewEncoder(&buf)
	if err := encodeXMLNodes(enc, doc.children); err != nil {
		return nil, false, err
	}
	if err := enc.Flush(); err != nil {
		return nil, false, err
	}
	return buf.Bytes(), true, nil
}

func encodeXMLNodes(enc *xml.Encoder, children []any) error {
	for _, ch := range children {
		switch t := ch.(type) {
		case *xmlNode:
			if err := enc.EncodeToken(t.start); err != nil {
				return err
			}
			if err := encodeXMLNodes(enc, t.children); err != nil {
				return err
			}
			if err := enc.EncodeToken(t.start.End()); err != nil {
				return err
			}
		case xml.Token:
			if err := enc.EncodeToken(t); err != nil {
				return err
			}
		}
	}
	return nil
}

// filterChildrenXML is filterChildrenJSON for an element.
func (r *hideRun) filterChildrenXML(n *xmlNode) int {
	removed := 0
	// Airings first, so Channel tags can be matched by the titles they teach.
	order := make([]*xmlNode, 0, len(n.children))
	for _, ch := range n.children {
		if e, ok := ch.(*xmlNode); ok && e.start.Name.Local == "Media" {
			order = append(order, e)
		}
	}
	for _, ch := range n.children {
		if e, ok := ch.(*xmlNode); ok && e.start.Name.Local != "Media" {
			order = append(order, e)
		}
	}
	drop := map[*xmlNode]bool{}
	for _, e := range order {
		if r.dropXML(e) {
			drop[e] = true
			removed++
		}
	}
	if removed > 0 && !r.countOnly {
		kept := n.children[:0:0]
		for _, ch := range n.children {
			if e, ok := ch.(*xmlNode); ok && drop[e] {
				continue
			}
			kept = append(kept, ch)
		}
		n.children = kept
		for _, sizeKey := range []string{"size", "totalSize"} {
			if v, err := strconv.Atoi(n.attr(sizeKey)); err == nil {
				n.setAttr(sizeKey, strconv.Itoa(max(0, v-removed)))
			}
		}
	}
	return removed
}

func (r *hideRun) dropXML(e *xmlNode) bool {
	name := e.start.Name.Local
	if r.entryHidden(name, e.attr) {
		r.count()
		return true
	}
	hadMedia := e.elementCount("Media")
	hadElements := e.elementCount("")
	before := r.removed
	r.filterChildrenXML(e)
	if r.countOnly || r.removed == before {
		return false
	}
	if hadMedia > 0 && e.elementCount("Media") == 0 {
		r.count()
		return true
	}
	if name == "SearchResult" && hadElements > 0 && e.elementCount("") == 0 {
		r.count()
		return true
	}
	return false
}

var tunerChannelsPathRE = regexp.MustCompile(`^/media/grabbers/devices/([^/]+)/channels/?$`)

// tunerDevice is the device whose channels path lists, or "".
func tunerDevice(path string) string {
	if m := tunerChannelsPathRE.FindStringSubmatch(path); m != nil {
		return m[1]
	}
	return ""
}

// hidePathCandidate reports whether a response on path can list channels:
// provider-scoped Live TV paths (guide, hubs, search, metadata), /livetv/
// (DVRs, sessions), the tuner's channels and DVR subscriptions.
func hidePathCandidate(path string) bool {
	return liveProviderPathRE.MatchString(path) ||
		strings.HasPrefix(path, "/livetv/") ||
		strings.HasPrefix(path, "/media/grabbers/") ||
		isMediaSubscriptionPath(path)
}

// hideCtxKey carries, on a proxied request, the caller's own token (before
// any elevation) and whether the owner asked for the friend view.
type hideCtxKey struct{}

type hideCtx struct {
	callerToken  string
	viewAsFriend bool
}

// noteHideCaller records the caller on req for the response step, and strips
// ViewAsHeader so PMS never sees it. Called by the Director.
func (p *Proxy) noteHideCaller(req *http.Request, originalToken string) {
	viewAs := strings.EqualFold(strings.TrimSpace(req.Header.Get(ViewAsHeader)), "friend")
	req.Header.Del(ViewAsHeader)
	if p.hidden == nil {
		return
	}
	hc := hideCtx{
		callerToken:  strings.TrimSpace(originalToken),
		viewAsFriend: viewAs && p.isOwnerToken(originalToken),
	}
	if hidePathCandidate(req.URL.EscapedPath()) && (hc.viewAsFriend || !p.isOwnerToken(originalToken)) {
		// A 304 would let the client reuse a body it cached unfiltered (from
		// before hiding, or as the owner's device): ask PMS for the whole
		// response, so it's filtered.
		req.Header.Del("If-None-Match")
		req.Header.Del("If-Modified-Since")
	}
	*req = *req.WithContext(context.WithValue(req.Context(), hideCtxKey{}, hc))
}

func (p *Proxy) isOwnerToken(token string) bool {
	owner := strings.TrimSpace(p.cfg.OwnerToken)
	token = strings.TrimSpace(token)
	return owner != "" && token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(owner)) == 1
}

// classifyCaller says who token belongs to, waiting up to classifyWait for a
// share-list fetch when it isn't settled yet. Still unsettled, it's treated
// like a shared user.
func (p *Proxy) classifyCaller(ctx context.Context, token string) viewerClass {
	token = strings.TrimSpace(token)
	if p.isOwnerToken(token) {
		return viewerOwner
	}
	if token == "" || p.viewers == nil {
		return viewerUnsettled
	}
	fp := tokenFingerprint(token)
	if c, settled := p.viewers.classify(fp, time.Now()); settled {
		return c
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fetchCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := p.viewers.refreshAccounts(fetchCtx, time.Now()); err != nil {
			p.logger.Printf("plexlabelproxy_hide: share list fetch failed: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(classifyWait):
	case <-ctx.Done():
	}
	if c, settled := p.viewers.classify(fp, time.Now()); settled {
		return c
	}
	return viewerUnsettled
}

// refuseHiddenChannel answers 404, without asking PMS, a request naming a
// hidden channel (a tune, say) from anyone but the owner and the owner's
// devices. It reports whether it answered.
func (p *Proxy) refuseHiddenChannel(w http.ResponseWriter, r *http.Request) bool {
	ids := channelTargetIDs(r)
	if len(ids) == 0 {
		return false
	}
	anyHidden := func() bool {
		for _, id := range ids {
			if p.hidden.channelHidden(id) {
				return true
			}
		}
		return false
	}
	if !anyHidden() {
		// A channel identifier Tunarr's pattern didn't predict: the DVR
		// channel map may name it (refetched at most every minute).
		p.hidden.refreshMap(r.Context())
		if !anyHidden() {
			return false
		}
	}
	token := inboundPlexToken(r)
	class := p.classifyCaller(r.Context(), token)
	if p.isOwnerToken(token) && strings.EqualFold(strings.TrimSpace(r.Header.Get(ViewAsHeader)), "friend") {
		class = viewerPreview
	}
	if class.seesHiddenChannels() {
		return false
	}
	// Not a bad attempt: the token may be fine for every other channel.
	p.auditElevation(r, "deny_hidden_channel", token, "channel hidden from shared users (-hide-live-channel) viewer="+class.String())
	http.NotFound(w, r)
	return true
}

// channelTargetIDs lists the channels a request names under
// /livetv/dvrs/<dvr>/channels/: in its own path, in any query value (a
// transcode's path, a play queue's uri or path, however escaped), and in a
// play queue's form or JSON body, as IsLiveTVStreamRequest reads them.
func channelTargetIDs(r *http.Request) []string {
	values := []string{r.URL.EscapedPath()}
	for _, vs := range r.URL.Query() {
		values = append(values, vs...)
	}
	if strings.HasPrefix(r.URL.EscapedPath(), "/playQueues") {
		if body, ok := readRequestBodyForClassification(r); ok {
			values = append(values, bodyStringValues(body)...)
		}
	}
	var ids []string
	for _, v := range values {
		for _, target := range targetPaths(v) {
			m := liveTVChannelPathRE.FindStringSubmatch(target)
			if m == nil {
				continue
			}
			ids = append(ids, m[1])
			if id, err := url.PathUnescape(m[1]); err == nil && id != m[1] {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// bodyStringValues is every value of a form body, or every string in a JSON
// body.
func bodyStringValues(body []byte) []string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		form, err := url.ParseQuery(string(trimmed))
		if err != nil {
			return nil
		}
		var out []string
		for _, vs := range form {
			out = append(out, vs...)
		}
		return out
	}
	var doc any
	if json.Unmarshal(trimmed, &doc) != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(node any) {
		switch n := node.(type) {
		case string:
			out = append(out, n)
		case map[string]any:
			for _, v := range n {
				walk(v)
			}
		case []any:
			for _, v := range n {
				walk(v)
			}
		}
	}
	walk(doc)
	return out
}

// targetPaths is the cleaned server paths a request value may name, decoded
// as liveTVTarget decodes it: a path, a provider-relative key, or the path in
// a server://<machine>/<provider>/<path> URI.
func targetPaths(v string) []string {
	var out []string
	for i := 0; i < 5; i++ {
		out = append(out, targetPathsRaw(v)...)
		next, err := url.QueryUnescape(v)
		if err != nil || next == v {
			next, err = url.PathUnescape(v)
			if err != nil || next == v {
				break
			}
		}
		v = next
	}
	return out
}

func targetPathsRaw(v string) []string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(strings.ToLower(v), "tv.plex.providers.epg.xmltv:") {
		if i := strings.IndexAny(v, "?#"); i >= 0 {
			v = v[:i]
		}
		return []string{path.Clean("/" + v)}
	}
	u, err := url.Parse(v)
	if err != nil {
		return nil
	}
	var out []string
	if strings.HasPrefix(u.Path, "/") {
		out = append(out, path.Clean(u.Path))
	}
	if u.Scheme != "" && u.Host != "" {
		rest := strings.TrimPrefix(u.Path, "/")
		if i := strings.Index(rest, "/"); i >= 0 {
			out = append(out, path.Clean(rest[i:]))
		}
	}
	return out
}

// hideLiveChannelEntries removes hidden channels from a Live TV response for
// anyone but the owner and the owner's devices, and checks what's left for
// any trace of them (leak_suspect). Runs in modifyResponse.
func (p *Proxy) hideLiveChannelEntries(resp *http.Response) error {
	if p.hidden == nil || resp == nil || resp.Request == nil || resp.Body == nil || resp.StatusCode != http.StatusOK {
		return nil
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	isJSON, isXML := strings.Contains(ct, "json"), strings.Contains(ct, "xml")
	if !isJSON && !isXML {
		return nil
	}
	path := resp.Request.URL.EscapedPath()
	onPath := hidePathCandidate(path)
	_, elevated := resp.Request.Context().Value(clientTokenContextKey{}).(string)
	if !onPath && !elevated {
		return nil
	}
	hc, _ := resp.Request.Context().Value(hideCtxKey{}).(hideCtx)
	class := p.classifyCaller(resp.Request.Context(), hc.callerToken)
	if hc.viewAsFriend {
		class = viewerPreview
	}
	if class.seesHiddenChannels() && !(p.cfg.HideLiveChannelsDebug && onPath) {
		return nil
	}

	if resp.ContentLength > maxScrubBodyBytes {
		return fmt.Errorf("%s response of %d bytes is too large to check for hidden channels", path, resp.ContentLength)
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.LimitReader(resp.Body, maxScrubBodyBytes+1), resp.Body}
	body, encoding, err := readBody(resp)
	if err != nil {
		return err
	}
	if len(body) > maxScrubBodyBytes {
		return fmt.Errorf("%s response is too large to check for hidden channels", path)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return restoreBody(resp, body, encoding)
	}

	if class.seesHiddenChannels() {
		// Debug: count what a shared user wouldn't see, change nothing.
		run := p.hidden.newRun(tunerDevice(path), true)
		if isJSON {
			_, _, _ = run.filterJSON(body)
		} else if looksLikeXML(body) {
			_, _, _ = run.filterXML(body)
		}
		p.logger.Printf("plexlabelproxy_hide: outcome=owner_kept path=%s viewer=%s present=%d", path, class, run.present)
		return restoreBody(resp, body, encoding)
	}

	if onPath && (p.hidden.needsMap() || tunerDevice(path) != "") {
		// The tuner's channels are named by number only: an identifier's
		// tuner channel comes from a current DVR channel map.
		p.hidden.refreshMap(resp.Request.Context())
	}
	run := p.hidden.newRun(tunerDevice(path), false)
	out := body
	if onPath {
		var filtered []byte
		var changed bool
		var ferr error
		if isJSON {
			filtered, changed, ferr = run.filterJSON(body)
		} else if looksLikeXML(body) {
			filtered, changed, ferr = run.filterXML(body)
		}
		if ferr != nil {
			// A body the filter can't parse might still list the channel.
			p.logger.Printf("plexlabelproxy_hide: outcome=unparsed path=%s viewer=%s err=%q", path, class, ferr.Error())
		} else if changed {
			out = filtered
			resp.Header.Del("X-Plex-Content-Original-Length")
			resp.Header.Del("X-Plex-Content-Compressed-Length")
			resp.Header.Set("Cache-Control", "private, no-store")
			if v, err := strconv.Atoi(resp.Header.Get("X-Plex-Container-Total-Size")); err == nil && run.rootRemoved > 0 {
				resp.Header.Set("X-Plex-Container-Total-Size", strconv.Itoa(max(0, v-run.rootRemoved)))
			}
			p.recordHideOutcome("removed")
			p.logger.Printf("plexlabelproxy_hide: outcome=removed path=%s viewer=%s removed=%d token_fp=%s", path, class, run.removed, tokenFingerprint(hc.callerToken))
		}
	}
	if leak := run.leakIn(out); leak != "" {
		p.recordHideOutcome("leak_suspect")
		p.logger.Printf("plexlabelproxy_hide: outcome=leak_suspect path=%s viewer=%s match=%q", path, class, leak)
	}
	return restoreBody(resp, out, encoding)
}

// HiddenLiveChannels resolves the hidden channels against Plex's channel map
// and describes them, for the startup log; "" when none are hidden.
func (p *Proxy) HiddenLiveChannels(ctx context.Context) string {
	if p.hidden == nil {
		return ""
	}
	p.hidden.refreshMap(ctx)
	return p.hidden.describe()
}

// dvrChannelMapFetcher reads the channel mappings of every DVR from PMS.
func dvrChannelMapFetcher(upstream *url.URL, token string, transport http.RoundTripper) func(context.Context) ([]channelMapping, error) {
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	return func(ctx context.Context) ([]channelMapping, error) {
		u := *upstream
		u.Path = "/livetv/dvrs"
		u.RawQuery = ""
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Plex-Token", token)
		req.Header.Set("Accept", "application/xml")
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("PMS /livetv/dvrs: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("PMS /livetv/dvrs: status %d", resp.StatusCode)
		}
		var doc struct {
			Dvr []struct {
				Device []struct {
					Key            string `xml:"key,attr"`
					ChannelMapping []struct {
						ChannelKey       string `xml:"channelKey,attr"`
						DeviceIdentifier string `xml:"deviceIdentifier,attr"`
						LineupIdentifier string `xml:"lineupIdentifier,attr"`
					} `xml:"ChannelMapping"`
				} `xml:"Device"`
			} `xml:"Dvr"`
		}
		if err := xml.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&doc); err != nil {
			return nil, fmt.Errorf("PMS /livetv/dvrs: %w", err)
		}
		var out []channelMapping
		for _, d := range doc.Dvr {
			for _, dev := range d.Device {
				for _, m := range dev.ChannelMapping {
					out = append(out, channelMapping{device: dev.Key, key: m.ChannelKey, deviceID: m.DeviceIdentifier, lineupID: m.LineupIdentifier})
				}
			}
		}
		return out, nil
	}
}
