# Current task (oliviacpu fork): `-live-viewers`

This fork only uses `plex-label-proxy` in Live TV elevation mode (see `FORK.md`). The upstream task this file
described before (API-Sports release closeout) belongs to upstream and isn't worked on here.

- **Goal:** monitoring tools can tell which shared user is behind a Live TV session the proxy elevated (Plex records
  it under the owner).
- **Scope:** one PR (#3): `-live-viewers` and `GET /iptvtunerr/live-viewers` (owner token or
  `IPTV_TUNERR_PROXY_LIVE_VIEWERS_KEY`), naming the Plex account behind each Live TV client whose start PMS accepted
  (`liveviewers.go`). Tracearr reads the list from its own fork; that's outside this repository.
- **Out of scope:** upstream features, other `iptv-tunerr` commands, sending this upstream.
- **Success criteria:** `liveviewers_test.go` passes; in production, a friend's Live TV client appears in the list
  under their account.
- **Verification:** `GOFLAGS=-mod=vendor go test ./internal/plexlabelproxy/` (the Unix socket test fails on macOS on
  `main` too: its temp path is over macOS's 104-byte limit); the tag build runs the suite on Linux.
- **Current status:** implemented and tested, in review. Next: merge, tag, deploy with `-live-viewers` and the key.
