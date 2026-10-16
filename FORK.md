# About this fork

A personal fork of [IPTV Tunerr](https://github.com/snapetech-org/iptvtunerr). Only one part of it is used here: the
`plex-label-proxy` command in Live TV elevation mode (`-elevate-live-tv`), which lets people a Plex server is shared
with watch its Live TV without joining the owner's Plex Home. See the upstream runbook,
[`docs/runbooks/plex-live-tv-entitlement-proxy.md`](docs/runbooks/plex-live-tv-entitlement-proxy.md).

The proxy holds the server owner's Plex token. The changes listed below tighten when that token is used and make sure
it never leaves the proxy.

## Rules for changes

- **One change per commit, each with a test** and an entry below. Keep commits upstream-shaped, so each could be sent
  upstream later on its own (`git cherry-pick <commit>` onto a branch from `upstream/main`).
- **Upstream behaviour stays the default.** Stricter behaviour that changes what upstream does is either a plain bug fix
  or sits behind a flag.
- **Tests** run in a container, with no local Go install needed:
  `docker run --rm -v "$PWD":/src -w /src -e GOFLAGS=-mod=vendor -e GOTOOLCHAIN=local golang:1.26 go test ./internal/plexlabelproxy/`
- **Tags** start with `oliviacpu-`, never `v`: upstream's `release.yml` publishes on `v*` tags.

## Image and GitHub Actions

- **Image:** `ghcr.io/oliviacpu/iptvtunerr:<tag>` and `:latest` (the newest tag build, which the deployment runs, so
  pushing a tag releases it) from `Dockerfile.fork-proxy`. It's the `iptv-tunerr` binary only,
  static, on distroless, for `plex-label-proxy`. It runs as root in the container, which rootless podman maps to the
  host account, so a socket the proxy creates belongs to that account.
- **Build:** `.github/workflows/fork-image.yml` runs on `oliviacpu-*` tags (or by hand on one). It runs the
  `plex-label-proxy` tests, then builds and pushes the image.
- **Upstream's workflows are disabled here** (`gh workflow disable <name> --repo oliviacpu/iptvtunerr`). Most of them
  publish to package registries (Chocolatey, Snap, Winget, AUR, COPR, PPA, Docker Hub) with secrets this fork doesn't
  have, and the rest would run CI and CodeQL on every push for nothing. After pulling upstream changes, check
  `gh workflow list --repo oliviacpu/iptvtunerr --all` for new upstream workflows and disable them too.

## Cloning on macOS

The repository has both `AGENTS.md` and `agents.md` (an alias pointing to the first). macOS file systems are
case-insensitive by default, so a clone can only hold one of them: git writes `AGENTS.md`, then `agents.md` over it,
and `git status` reports `AGENTS.md` as modified. After cloning, run:

```bash
git update-index --skip-worktree AGENTS.md agents.md
```

Then never commit either file from a macOS clone.

## License

IPTV Tunerr is AGPL-3.0-or-later. Anyone using a modified copy over a network is entitled to its source; this public
fork is where it lives.

## Changes in this fork

Newest first.

<!-- Entry format:
### Short title (YYYY-MM-DD, commit)
- **What it fixes:** the problem.
- **Cause:** what in the code produced it.
- **Fix:** what changed, with the files.
- **Upstream:** not sent / issue or PR link.
-->

### Hide a Live TV channel from shared users: `-hide-live-channel` (2026-10-16)

- **What it fixes:** the owner wants a test channel in Plex's Live TV that shared users don't see or tune. Plex has
  no per-user channel list: everyone with Live TV gets the DVR's whole lineup.
- **Cause:** shared users get Live TV only through the proxy, which passed Plex's channel lists, guide and tunes
  through unchanged.
- **Fix:**
  - `hidechannels.go`: with `-hide-live-channel` (`Config.HideLiveChannels`, numbers or guide identifiers), Live TV
    responses for anyone but the owner and the owner's devices lose the hidden channels' entries, in JSON (a generic
    walk with `UseNumber`, re-encoded only when changed) and XML (a small element tree, declaration kept): lineup
    `Channel`s, guide and hub airings (`Media`; a program left with none goes), `Channel` tags, `SearchResult`s,
    `ChannelMapping`s and `DeviceChannel`s, with `size`, `totalSize` and `X-Plex-Container-Total-Size` lowered,
    `X-Plex-Content-*-Length` dropped and `Cache-Control: private, no-store`. Gzip is kept. A request naming a hidden
    channel under `/livetv/dvrs/<dvr>/channels/`, in its path, any query value or a play queue's body (the nested
    targets `IsLiveTVStreamRequest` elevates), gets 404 without reaching PMS (`deny_hidden_channel`, not a bad
    attempt). A number hides that number on every DVR: it covers Tunarr's identifier (`C<n>.<digit sum>.tunarr.com`)
    and the identifiers the DVR channel map (`/livetv/dvrs`, cached, refetched at most once a minute) and responses
    pair with it. An identifier hides that channel only; its tuner entry (the tuner's channel list names channels by
    number only) once the map pairs it with its device's channel, rereading the map (at most once a minute) before a
    tuner's channel list is filtered and until it pairs.
  - Who's the owner: the owner token, or a token missing from a plex.tv share list fetched after it was first seen
    (the owner's own apps); a token on a share list is a shared user's for good. Tokens are remembered 24 hours
    after they were last seen, 1000 at most. Unsettled callers and requests without a token get the shared view,
    after waiting up to 2 seconds for a share-list fetch. `liveviewers.go`'s tracker does this (`classify`); it's
    created for either feature, and only a trusted list settles anyone: one that keeps everyone the last trusted list
    had, or the first or a shrunken list once the next fetch returns the same one (plex.tv may answer partly).
  - Checking it: `plexlabelproxy_hide: outcome=removed|leak_suspect|unparsed` lines and `hidden_removed=`,
    `hidden_refused=`, `hidden_leak_suspect=` in the summary; `-hide-live-channel-debug` logs `outcome=owner_kept
    present=<n>` for the owner's responses; the owner token with `X-Iptvtunerr-View-As: friend` gets the shared
    view (the header is stripped before PMS, always).
  - `proxy.go` and `cmd_plex_label_proxy.go`: config, the repeatable flag, the startup check (owner token and
    `-elevate-live-tv` or `-elevate-all`) and log line. `-live-viewers`' path and recording stay tied to its own
    flag. **Docs:** the runbook's "Hide channels from shared users".
- **Test:** `hidechannels_test.go` with anonymised fixtures in `testdata/hidechannels/` (channels 1, 2 and 99, a
  program only on 99 and one on 2 and 99): a shared user never sees 99 in any shape, JSON or XML, gzipped or not, with
  sizes, headers and Content-Length right, and a program on 2 and 99 keeps its channel-2 airing; the owner's and the
  owner's device's responses are byte-identical to the proxy without the flag; no token, or an owner device while
  plex.tv is down, is hidden, and settles once plex.tv answers, then stays settled through a failed fetch; a shared
  user dropped from a later list stays hidden; a first, partial or empty list isn't trusted until repeated; the tune
  of 99 by identifier or number gets 404, never reaches PMS and doesn't block the user, while channel 2 and the
  owner's and owner device's 99 pass; a tune of 99 nested in a transcode's path or a play queue's uri (query, form or
  JSON body), escaped once or twice, gets 404 too; an identifier from the DVR channel map, at startup and lazily, with
  the map cached; a guide identifier hides its tuner channel once the map pairs it, on its own device only, and leaves
  another DVR's same number alone; an identifier's tuner pairing follows a remap; an owner device in use stays
  remembered past 24 hours; the preview header works for the owner token only and never reaches PMS; off by default; a
  response without 99 is byte-identical; an empty body is skipped; an unknown shape naming 99 is flagged
  `leak_suspect`; debug counts without changing; no raw token in any test's logs. `cmd_plex_label_proxy_test.go`: the
  flag repeats and splits commas.
- **Upstream:** not sent.

### Who is watching Live TV: `-live-viewers` (2026-10-16)

- **What it fixes:** Plex records a shared user's Live TV under the owner, because the proxy tuned it with the owner
  token. Tracearr showed a friend's Live TV as the owner's, or nothing to tell them apart.
- **Cause:** nothing outside the proxy knows which account was behind an elevated tune.
- **Fix:**
  - `liveviewers.go`: with `-live-viewers` (`Config.LiveViewers`), each elevated Live TV start (tune, transcode
    start, play queue) that PMS accepts (2xx) records its client identifier against the token's fingerprint (noted on
    the request, recorded in `modifyResponse` once PMS has answered). plex.tv's share list for the
    server (`/api/servers/<machineIdentifier>/shared_servers`, owner token) gives each shared user's token for this
    server, so its fingerprints name the accounts. `GET /iptvtunerr/live-viewers` with the owner token lists
    `clientIdentifier`, `plexAccountId`, `username`, `lastSeen`; anything else gets 404 and never reaches PMS. Off,
    the path goes to PMS as before.
  - `IPTV_TUNERR_PROXY_LIVE_VIEWERS_KEY` (`Config.LiveViewersKey`, 24+ characters, environment only) also opens the
    list to `Authorization: Bearer <key>`, so Tracearr can read it without holding the owner token.
  - The share list is fetched when the list is asked for: at most every 10 minutes, or after a minute when a newly
    seen token isn't in it. Clients are kept 12 hours after their last Live TV start, 1000 at most. Only token
    fingerprints are kept. A client identifier is kept only if it's printable ASCII and at most 128 characters
    (Plex's are 20-40); the client supplies it.
  - A newly seen client is logged once, after the share list is refreshed if its token isn't known yet (in the
    background, not on the response path), so the log names the account. Concurrent refreshes share one fetch and
    wait for it. An account still unknown is only written off as an owner device once a share list fetched after the
    client was first seen lacks it; until then the lookup is retried (3 tries, a minute apart), and if plex.tv keeps
    failing, the client's next Live TV start tries again. At most 16 such lookups run at once; past that, the
    client's line waits for its next start.
  - The response carries the server's `machineIdentifier` (from the share list fetch), so a tool watching several
    servers applies it to the right one. A start with the owner token itself forgets that client's earlier mapping.
  - `cmd_plex_label_proxy.go`: the flag. **Docs:** the runbook's "Who is watching Live TV".
- **Test:** `liveviewers_test.go`: a friend's tune names account 42 and the owner's device stays unnamed, with no
  token in the response or logs; browsing the guide records nothing; without the owner token it's a 404 that neither
  reaches PMS nor fetches the share list; off by default the path is proxied; the share list refresh rules and the
  12-hour expiry; the key opens the list but a wrong, short or misplaced key doesn't, and it elevates nothing; a
  start PMS refused or never got isn't recorded; an identifier with a control character or over 128 characters isn't
  kept, and a kept one is quoted in the log; the new-viewer log names the account even before anyone read the list;
  the response names the server; an owner-token start clears a stale mapping; concurrent refreshes share one fetch;
  a plex.tv failure delays the viewer line to the next start instead of writing it as unknown; a friend shared after
  the last fetch is named on the retry, not written off as unknown; lookups are bounded.
- **Upstream:** not sent.

### A Plex hiccup no longer locks friends out (2026-10-16)

- **What it fixes:** while Plex restarted, a shared user's app and the owner's own devices were refused Live TV, then
  their addresses were blocked for 30 minutes (seen 2026-10-16: three addresses blocked within a minute of a Plex
  restart). Any Plex request slower than 5 s, during a library scan for example, could do the same. Friends saw it as
  Live TV failing and dropping.
- **Cause:** `PlexTokenAuthorizer.check` returned "not authorized" for every failure to ask Plex (connection refused,
  timeout, 5xx), and that answer was cached for 5 minutes. Each refusal then counted as a bad elevation attempt:
  a 2-minute cooldown, and a 30-minute block after 5 of them. Separately, `canElevate` and `blockBypassAllowed`
  called the authorizer twice per check (the plain then the detailed form), so every check was logged as a cache
  hit, and an uncached failure would have asked Plex twice.
- **Fix:**
  - `authorizer.go`: `check` reports whether Plex gave a verdict. Only 2xx (allowed) and 401/403 (refused) are
    cached. Otherwise the decision is `Unavailable`: a token Plex accepted in the last 24 hours stays allowed
    (rechecked every 30 s), and any other token is refused without caching.
  - `proxy.go`: an `Unavailable` refusal is logged as `deny_auth_unavailable` and isn't a bad attempt (no cooldown,
    no block); an `Unavailable` allow is logged as `allow_auth_unavailable`. The summary line gains
    `auth_unavailable=`. The authorizer is asked once per check (`authorize`).
  - **Docs:** the runbook's audit outcomes.
- **Test:** `authorizer_test.go`: a 503 isn't cached; a 401 is; a recently allowed token survives Plex being
  unreachable, but not past 24 hours or after a 401; and through the proxy, more Plex outages than the block threshold
  neither block the source nor start a cooldown, and the token is elevated as soon as Plex answers.
- **Upstream:** not sent.

### Listen on a private Unix socket (2026-10-14)

- **What it fixes:** the proxy could only listen on TCP, and it trusts `X-Forwarded-For` from any loopback peer. On a
  host where other accounts share `127.0.0.1`, any of them could connect, set their own forwarded address (sources
  for abuse blocking and session binding), and use the proxy as if they were the front end.
- **Cause:** `ListenAndServe` always called `net.Listen("tcp", …)`.
- **Fix:**
  - **Socket:** `-listen unix:/path` creates a Unix socket with `-socket-mode` (default `0600`,
    `Config.SocketMode`). A stale socket at the path is replaced; any other file is refused. The socket is removed on
    shutdown.
  - **Trust:** requests on it are marked through `http.Server.ConnContext`, and `trustedHeader` trusts their forwarding
    headers like a loopback peer's (`fromPrivateSocket`).
  - **Docs:** the runbook's Run section.
- **Test:** `socket_test.go`:
  - serving over the socket, with mode 600, a stale socket replaced, and the socket gone after shutdown;
  - a regular file at the path is refused and left untouched;
  - forwarded addresses are trusted only from socket or loopback peers.
- **Upstream:** not sent.

### A switch to turn off token-less session recovery (2026-10-14)

- **What it fixes:** with `-neutralize-owner-history` or `-elevate-all`, a Live TV request carrying no token borrows
  the token of a session tracked in the last 8 hours. It's matched by session or client identifier plus source address,
  then elevated to the owner. The identifiers aren't secret, and the source comes from `X-Forwarded-For`, which the
  proxy believes from any loopback connection. On a host whose loopback other users share, another account could ride
  a shared user's session.
- **Cause:** recovery has no off switch; it's tied to session tracking.
- **Fix:** `-no-session-token-recovery` (`Config.NoSessionTokenRecovery`): a tokenless request is never elevated. The
  default is unchanged. Documented in the runbook.
- **Test:** `session_recovery_test.go`, on (the tokenless segment gets the owner token) and off (no token).
- **Upstream:** not sent.

### Only canonical request paths are elevated (2026-10-14)

- **What it fixes:** a shared user could reach any owner-only Plex endpoint through a request path that starts like
  Live TV but resolves elsewhere: `/livetv/../:/prefs`, `/livetv/%2e%2e/library/sections`, `/livetv/..%2flibrary/…`,
  `/media/grabbers/../../library/sections/1/refresh`, `/media/subscriptions/../../:/prefs`. All were classified as
  Live TV and sent with the owner token. Some Plex `GET` endpoints change state (library refresh, scrobble).
- **Cause:** `IsLiveTVRequest` and friends matched prefixes of the raw escaped path, and neither Go's server nor the
  reverse proxy resolves dot segments, so Plex receives the path as written.
- **Fix:** `canonicalRequestPath` in `entitlement.go`: only absolute paths with no `.`, `..` or empty segments (one
  trailing slash allowed) and no `%2e`, `%2f`, `%5c` or `\` can be classified as Live TV. Applied in
  `IsLiveTVRequest`, `IsLiveTVDiscoveryRequest`, `IsLiveTVStreamRequest` and `isLiveTVSubscriptionRequest`. Anything
  else goes to Plex with the client's own token.
- **Test:** `canonical_path_test.go`. Before the fix, all ten paths were elevated and the round trip reached Plex with
  the owner token.
- **Upstream:** not sent.

### The owner token never leaves in a response (2026-10-14)

- **What it fixes:** responses to elevated requests went back to the client unchanged. If Plex echoed the token a
  request arrived with (a redirect `Location`, an HLS playlist, a `key` URL in XML or JSON), the shared user received
  the owner's token. The runbook said the token is not returned to the client, but nothing enforced it.
- **Cause:** `modifyResponse` only rewrote labels and `allowTuners` hints.
- **Fix:**
  - **Marking:** the director marks every request that reaches Plex with the owner token on someone else's behalf. It
    stores the client's own token in the request context. This covers any elevation path, including `-elevate-all`.
  - **Scrubbing:** `scrubOwnerToken`, the first step of `modifyResponse`, replaces the owner token with the client's in
    response headers and text bodies (XML, JSON, HTML, text, HLS playlists; gzip included).
  - **What isn't read:** media bodies, WebSocket upgrades, event streams, and untyped bodies of unknown length.
  - **Size limit:** an elevated text response over 64 MiB is refused rather than passed on unchecked.
- **Test:** `scrub_test.go`. A fake Plex echoes the token into a redirect, XML, a playlist and a gzip body; the client
  sees only its own token. Library responses and the owner's own responses are untouched. Before the fix, all four
  echoed the owner token to the client.
- **Upstream:** not sent.

### Flags to keep recordings and the server summary out of elevation (2026-10-14)

- **What it fixes:** with `-elevate-live-tv`, shared users got the owner token for DVR subscription requests (seeing,
  creating, editing and deleting recordings as the owner). They also got it for `/` and `/identity` whenever their
  `Referer` named a Live TV page. The owner's view of those includes account details, and the `Referer` is
  client-supplied.
- **Cause:** both are part of upstream's Live TV classification, with no way to turn them off.
- **Fix:** two opt-in flags. Upstream's default is unchanged.
  - `-no-elevate-subscriptions` (`Config.NoElevateSubscriptions`)
  - `-no-elevate-root-identity` (`Config.NoElevateRootIdentity`)

  `Proxy.liveTVElevationAllowed` applies them after classification, with `deny_subscription` and `deny_root_identity`
  audit lines. Documented in the runbook's Request Classification and Run sections.
- **Test:** `opt_out_test.go`, each flag on and off against the same requests.
- **Upstream:** not sent.

### Live TV elevation only for real Live TV targets (2026-10-14)

- **What it fixes:** a shared user could borrow the owner token for library content. A transcode, play queue or
  subscription request counted as Live TV if a parameter merely contained `/livetv/` somewhere, for example
  `path=/library/metadata/123?x=/livetv/` (query), `…#/livetv/` (fragment), `/livetv/../library/metadata/123`
  (dot-dot), or any other form or JSON field mentioning it. Users without access to every library could reach the rest
  as the owner.
- **Cause:** `liveTVText` in `internal/plexlabelproxy/entitlement.go` matched substrings of each parameter value and,
  for request bodies, of the whole body.
- **Fix:**
  - **Parameters:** `liveTVTarget` accepts a value only if, decoded, it is an XMLTV guid or provider key, or its
    cleaned path (query and fragment ignored) is under `/livetv/` or an XMLTV EPG provider, directly or as the path of
    a `server://` URI.
  - **Bodies:** form and JSON bodies are checked field by field (the same hint keys); the whole-body text match is
    gone.
  - **Referer:** the `Referer` check for `/` and `/identity` is unchanged; see the next change.
- **Test:** `entitlement_strict_test.go`. The crafted requests above aren't elevated, real Live TV targets still are,
  and a library transcode mentioning `/livetv/` reaches Plex with the user's token. Before the fix, ten of the crafted
  requests were elevated.
- **Upstream:** not sent.
