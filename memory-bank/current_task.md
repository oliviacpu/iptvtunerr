# Current task (oliviacpu fork): `-hide-live-channel`

This fork only uses `plex-label-proxy` in Live TV elevation mode (see `FORK.md`). The upstream task this file
described before (API-Sports release closeout) belongs to upstream and isn't worked on here.

- **Goal:** the owner can keep a Live TV channel (a test channel, 99) that shared users neither see nor tune, while
  the owner and the owner's own apps do.
- **Scope:** one PR: `-hide-live-channel` and `-hide-live-channel-debug` (`hidechannels.go`), the share-list
  classification in `liveviewers.go`'s tracker, the owner-only `X-Iptvtunerr-View-As: friend` preview, docs.
- **Out of scope:** upstream features, other `iptv-tunerr` commands, filtering Plex's WebSocket notifications,
  sending this upstream.
- **Success criteria:** `hidechannels_test.go` passes; in production, the owner's preview of the lineup and guide has
  no `C99.114`, a preview tune of 99 gets 404, the owner's guide shows 99 (`owner_kept present>0` with debug), and
  shared users' `outcome=removed` lines appear.
- **Verification:** `GOFLAGS=-mod=vendor go test ./internal/plexlabelproxy/` (the Unix socket test fails on macOS on
  `main` too: its temp path is over macOS's 104-byte limit); the tag build runs the suite on Linux.
- **Current status:** implemented and tested, in review. Next: merge, tag, deploy with `-hide-live-channel=99`
  before channel 99 exists in Plex.
