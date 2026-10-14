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
