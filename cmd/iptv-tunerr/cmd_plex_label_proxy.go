package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/snapetech/iptvtunerr/internal/config"
	"github.com/snapetech/iptvtunerr/internal/plexlabelproxy"
)

// plexLabelProxyCommands registers `plex-label-proxy`, a reverse proxy in
// front of PMS that rewrites Live TV provider labels and can optionally
// elevate only Plex Live TV requests to the PMS owner token.
//
// Why a separate process: Plex stamps every Live TV provider's friendlyName
// with the PMS server's own friendly name, and exposes no API to set per-DVR
// labels. The only place to fix this is on the wire.
func plexLabelProxyCommands() []commandSpec {
	cmd := flag.NewFlagSet("plex-label-proxy", flag.ExitOnError)
	listen := cmd.String("listen", "", "Listen address for the proxy: host:port, or unix:/path/to.sock for a Unix socket (default: IPTV_TUNERR_PLEX_LABEL_PROXY_LISTEN or 127.0.0.1:33240)")
	socketMode := cmd.String("socket-mode", "0600", "Permission mode (octal) of the Unix socket when -listen is unix:/path")
	upstream := cmd.String("upstream", "", "Upstream PMS base URL (default: -plex-url, IPTV_TUNERR_PMS_URL, or PLEX_HOST)")
	token := cmd.String("token", "", "PMS token used to query /livetv/dvrs (default: IPTV_TUNERR_PMS_TOKEN or PLEX_TOKEN)")
	ownerToken := cmd.String("owner-token", "", "Owner PMS token used only when -elevate-live-tv is enabled (default: IPTV_TUNERR_PMS_OWNER_TOKEN, PLEX_OWNER_TOKEN, or -token/env token)")
	plexURL := cmd.String("plex-url", "", "Convenience alias for -upstream")
	stripPrefix := cmd.String("strip-prefix", "iptvtunerr-", "Prefix to strip from DVR lineup titles when forming labels (\"\" disables)")
	refreshSec := cmd.Int("refresh-seconds", 30, "TTL for the cached /livetv/dvrs label map")
	spoofIdentity := cmd.Bool("spoof-identity", false, "Also rewrite root MediaContainer friendlyName for Plex Web (carries identity-cache risk; see runbook)")
	elevateAll := cmd.Bool("elevate-all", false, "Inject owner token on every proxied request (full token spoof — Live TV works, watch history is shared with owner)")
	elevateLiveTV := cmd.Bool("elevate-live-tv", false, "Use owner token only for Live TV classified paths; normal library paths pass through as the user")
	elevateDiscoveryOnly := cmd.Bool("elevate-discovery-only", false, "With -elevate-live-tv: only elevate browse/EPG paths, not stream start")
	userHeader := cmd.Bool("user-header", false, "With -elevate-live-tv: inject X-Plex-User header with the original client token when elevating")
	neutralizeOwnerHistory := cmd.Bool("neutralize-owner-history", false, "With -elevate-live-tv: fire /:/unscrobble under the owner token for each Live TV timeline event")
	noElevateSubscriptions := cmd.Bool("no-elevate-subscriptions", false, "With -elevate-live-tv: never use the owner token for DVR subscription (recording) requests")
	noElevateRootIdentity := cmd.Bool("no-elevate-root-identity", false, "With -elevate-live-tv: never use the owner token for / and /identity (the owner's view includes account details)")
	noSessionTokenRecovery := cmd.Bool("no-session-token-recovery", false, "With -elevate-live-tv: never elevate a request that carries no Plex token, even if it matches a tracked session")
	liveViewers := cmd.Bool("live-viewers", false, "With -elevate-live-tv: remember which Plex account is behind each client that starts Live TV, and list them at "+plexlabelproxy.LiveViewersPath+" for callers with the owner token or IPTV_TUNERR_PROXY_LIVE_VIEWERS_KEY (Plex records those sessions under the owner)")
	abuseThreshold := cmd.Int("abuse-block-threshold", envInt("IPTV_TUNERR_PROXY_ABUSE_THRESHOLD", 0), "Failed Live TV elevation attempts per source before temporary block (default 5; env IPTV_TUNERR_PROXY_ABUSE_THRESHOLD)")
	abuseWindow := cmd.Duration("abuse-block-window", envDuration("IPTV_TUNERR_PROXY_ABUSE_WINDOW", 0), "Rolling window for failed elevation attempts (default 5m; env IPTV_TUNERR_PROXY_ABUSE_WINDOW)")
	abuseDuration := cmd.Duration("abuse-block-duration", envDuration("IPTV_TUNERR_PROXY_ABUSE_BLOCK_DURATION", 0), "Temporary block duration after threshold (default 30m; env IPTV_TUNERR_PROXY_ABUSE_BLOCK_DURATION)")
	abuseStateFile := cmd.String("abuse-block-state-file", strings.TrimSpace(os.Getenv("IPTV_TUNERR_PROXY_ABUSE_STATE_FILE")), "Optional JSON state file for persisted bad-source blocks (env IPTV_TUNERR_PROXY_ABUSE_STATE_FILE)")
	badAuthCooldown := cmd.Duration("bad-auth-cooldown", envDuration("IPTV_TUNERR_PROXY_BAD_AUTH_COOLDOWN", 0), "Cooldown for denied source+token authorization checks (default 2m; negative disables; env IPTV_TUNERR_PROXY_BAD_AUTH_COOLDOWN)")
	var hideLiveChannels stringListFlag
	cmd.Var(&hideLiveChannels, "hide-live-channel", "With -elevate-live-tv: hide this Live TV channel (number, e.g. 99, or guide id) from shared users; only the owner and the owner's devices see and tune it. Repeat or comma-separate for several")
	hideLiveChannelDebug := cmd.Bool("hide-live-channel-debug", false, "With -hide-live-channel: also log, for the owner's requests, how many hidden entries a response had (outcome=owner_kept)")
	auditSummaryInterval := cmd.Duration("audit-summary-interval", envDuration("IPTV_TUNERR_PROXY_AUDIT_SUMMARY_INTERVAL", 0), "Aggregate audit summary interval (default 5m; negative disables; env IPTV_TUNERR_PROXY_AUDIT_SUMMARY_INTERVAL)")

	return []commandSpec{
		{
			Name:    "plex-label-proxy",
			Section: "Lab/ops",
			Summary: "Reverse-proxy PMS for Live TV labels and optional entitlement elevation",
			FlagSet: cmd,
			Run: func(_ *config.Config, args []string) {
				_ = cmd.Parse(args)
				runPlexLabelProxy(*listen, *upstream, *plexURL, *token, *ownerToken, *stripPrefix, *refreshSec, *spoofIdentity, *elevateAll, *elevateLiveTV, *elevateDiscoveryOnly, *userHeader, *neutralizeOwnerHistory, *noElevateSubscriptions, *noElevateRootIdentity, *noSessionTokenRecovery, *liveViewers, hideLiveChannels, *hideLiveChannelDebug, *socketMode, *abuseThreshold, *abuseWindow, *abuseDuration, *abuseStateFile, *badAuthCooldown, *auditSummaryInterval)
			},
		},
	}
}

func runPlexLabelProxy(listen, upstream, plexURL, token, ownerToken, stripPrefix string, refreshSec int, spoofIdentity, elevateAll, elevateLiveTV, elevateDiscoveryOnly, userHeader, neutralizeOwnerHistory, noElevateSubscriptions, noElevateRootIdentity, noSessionTokenRecovery, liveViewers bool, hideLiveChannels []string, hideLiveChannelDebug bool, socketMode string, abuseThreshold int, abuseWindow, abuseDuration time.Duration, abuseStateFile string, badAuthCooldown, auditSummaryInterval time.Duration) {
	// Always consult env/aliases so a flag for one field doesn't suppress
	// fallback resolution for the other.
	resolved, resolvedToken := resolvePlexAccess(plexURL, token)
	if strings.TrimSpace(upstream) == "" {
		upstream = resolved
	}
	if strings.TrimSpace(token) == "" {
		token = resolvedToken
	}
	if strings.TrimSpace(listen) == "" {
		listen = strings.TrimSpace(os.Getenv("IPTV_TUNERR_PLEX_LABEL_PROXY_LISTEN"))
	}
	if strings.TrimSpace(listen) == "" {
		listen = "127.0.0.1:33240"
	}
	upstream = strings.TrimSpace(upstream)
	if upstream == "" {
		log.Print("plex-label-proxy: need -upstream (or -plex-url, IPTV_TUNERR_PMS_URL, or PLEX_HOST)")
		os.Exit(1)
	}
	if strings.TrimSpace(token) == "" {
		log.Print("plex-label-proxy: need -token (or IPTV_TUNERR_PMS_TOKEN / PLEX_TOKEN) to read /livetv/dvrs")
		os.Exit(1)
	}
	mode, err := strconv.ParseUint(strings.TrimSpace(socketMode), 8, 32)
	if err != nil || mode > 0o777 {
		log.Printf("plex-label-proxy: invalid -socket-mode %q (want octal, e.g. 0600)", socketMode)
		os.Exit(1)
	}
	ownerToken = resolvePlexOwnerToken(ownerToken, token)
	if (elevateAll || elevateLiveTV) && strings.TrimSpace(ownerToken) == "" {
		log.Print("plex-label-proxy: need -owner-token (or IPTV_TUNERR_PMS_OWNER_TOKEN / PLEX_OWNER_TOKEN) when -elevate-all or -elevate-live-tv is enabled")
		os.Exit(1)
	}
	if len(hideLiveChannels) > 0 && !(elevateAll || elevateLiveTV) {
		log.Print("plex-label-proxy: -hide-live-channel needs -elevate-live-tv (shared users only get Live TV through the proxy then)")
		os.Exit(1)
	}
	var tokenAuthorizer plexlabelproxy.TokenAuthorizer
	if elevateAll || elevateLiveTV {
		authorizer, err := plexlabelproxy.NewPlexTokenAuthorizer(upstream, ownerToken, nil)
		if err != nil {
			log.Printf("plex-label-proxy: build token authorizer: %v", err)
			os.Exit(1)
		}
		tokenAuthorizer = authorizer
	}

	ttl := time.Duration(refreshSec) * time.Second
	cache := plexlabelproxy.NewLabelMapCache(upstream, token, stripPrefix, ttl, nil)
	if err := cache.Refresh(); err != nil {
		log.Printf("plex-label-proxy: initial /livetv/dvrs fetch failed: %v (will retry on first request)", err)
	} else {
		log.Printf("plex-label-proxy: loaded %d DVR label(s)", len(cache.Get()))
	}

	proxy, err := plexlabelproxy.New(plexlabelproxy.Config{
		Upstream:               upstream,
		Token:                  token,
		OwnerToken:             ownerToken,
		ElevateAll:             elevateAll,
		ElevateLiveTV:          elevateLiveTV,
		ElevateDiscoveryOnly:   elevateDiscoveryOnly,
		UserHeader:             userHeader,
		NeutralizeOwnerHistory: neutralizeOwnerHistory,
		NoElevateSubscriptions: noElevateSubscriptions,
		NoElevateRootIdentity:  noElevateRootIdentity,
		NoSessionTokenRecovery: noSessionTokenRecovery,
		LiveViewers:            liveViewers,
		HideLiveChannels:       hideLiveChannels,
		HideLiveChannelsDebug:  hideLiveChannelDebug,
		// read from the environment only, so it never shows in process listings
		LiveViewersKey:       strings.TrimSpace(os.Getenv("IPTV_TUNERR_PROXY_LIVE_VIEWERS_KEY")),
		SocketMode:           os.FileMode(mode),
		Labels:               cache,
		SpoofIdentity:        spoofIdentity,
		TokenAuthorizer:      tokenAuthorizer,
		AbuseBlockThreshold:  abuseThreshold,
		AbuseBlockWindow:     abuseWindow,
		AbuseBlockDuration:   abuseDuration,
		AbuseBlockStateFile:  abuseStateFile,
		BadAuthCooldown:      badAuthCooldown,
		AuditSummaryInterval: auditSummaryInterval,
	})
	if err != nil {
		log.Printf("plex-label-proxy: %v", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if len(hideLiveChannels) > 0 {
		mapCtx, mapCancel := context.WithTimeout(ctx, 15*time.Second)
		log.Printf("plex-label-proxy: hiding Live TV channel(s) from shared users: %s", proxy.HiddenLiveChannels(mapCtx))
		mapCancel()
	}

	log.Printf("plex-label-proxy: starting on %s -> %s (elevate_all=%v elevate_live_tv=%v discovery_only=%v neutralize_owner=%v no_elevate_subscriptions=%v no_elevate_root_identity=%v no_session_token_recovery=%v live_viewers=%v spoof_identity=%v strip_prefix=%q)",
		listen, upstream, elevateAll, elevateLiveTV, elevateDiscoveryOnly, neutralizeOwnerHistory, noElevateSubscriptions, noElevateRootIdentity, noSessionTokenRecovery, liveViewers, spoofIdentity, stripPrefix)
	if err := proxy.ListenAndServe(ctx, listen); err != nil {
		log.Printf("plex-label-proxy: server exited: %v", err)
		os.Exit(1)
	}
	log.Print("plex-label-proxy: shutdown complete")
}

func envInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("invalid %s=%q; using %d", name, raw, fallback)
		return fallback
	}
	return n
}

func envDuration(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err == nil {
		return d
	}
	seconds, serr := strconv.Atoi(raw)
	if serr != nil {
		log.Printf("invalid %s=%q; using %s", name, raw, fallback)
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

func resolvePlexOwnerToken(flagOwnerToken, fallbackToken string) string {
	if t := strings.TrimSpace(flagOwnerToken); t != "" {
		return t
	}
	if t := strings.TrimSpace(os.Getenv("IPTV_TUNERR_PMS_OWNER_TOKEN")); t != "" {
		return t
	}
	if t := strings.TrimSpace(os.Getenv("PLEX_OWNER_TOKEN")); t != "" {
		return t
	}
	return strings.TrimSpace(fallbackToken)
}

// stringListFlag is a repeatable string flag; each use may also list several
// values separated by commas.
type stringListFlag []string

func (f *stringListFlag) String() string { return strings.Join(*f, ",") }

func (f *stringListFlag) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*f = append(*f, part)
		}
	}
	return nil
}
