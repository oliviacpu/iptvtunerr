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

## GitHub Actions

- **Off on this fork** (repository setting) until it has its own image workflow.
- **Upstream's workflows aren't used here.** Most of them publish to package registries (Chocolatey, Snap, Winget, AUR,
  COPR, PPA, Docker Hub) with secrets this fork doesn't have.
- **When the fork's image workflow is added:** turn Actions back on, then disable every other workflow
  (`gh workflow disable <name> --repo oliviacpu/iptvtunerr`).

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
