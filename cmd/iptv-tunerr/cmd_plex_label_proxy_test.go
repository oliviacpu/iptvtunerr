package main

import (
	"flag"
	"slices"
	"testing"
)

func TestResolvePlexOwnerToken(t *testing.T) {
	t.Setenv("IPTV_TUNERR_PMS_OWNER_TOKEN", "")
	t.Setenv("PLEX_OWNER_TOKEN", "")
	if got := resolvePlexOwnerToken(" flag-owner ", "fallback"); got != "flag-owner" {
		t.Fatalf("flag owner got %q", got)
	}

	t.Setenv("IPTV_TUNERR_PMS_OWNER_TOKEN", " env-owner ")
	if got := resolvePlexOwnerToken("", "fallback"); got != "env-owner" {
		t.Fatalf("IPTV_TUNERR_PMS_OWNER_TOKEN got %q", got)
	}

	t.Setenv("IPTV_TUNERR_PMS_OWNER_TOKEN", "")
	t.Setenv("PLEX_OWNER_TOKEN", " plex-owner ")
	if got := resolvePlexOwnerToken("", "fallback"); got != "plex-owner" {
		t.Fatalf("PLEX_OWNER_TOKEN got %q", got)
	}

	t.Setenv("PLEX_OWNER_TOKEN", "")
	if got := resolvePlexOwnerToken("", " fallback "); got != "fallback" {
		t.Fatalf("fallback got %q", got)
	}
}

func TestStringListFlag_RepeatedAndCommaSeparated(t *testing.T) {
	var hide stringListFlag
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.Var(&hide, "hide-live-channel", "")
	if err := fs.Parse([]string{"-hide-live-channel=99", "-hide-live-channel", " 98 , ,C5.53.tunarr.com"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"99", "98", "C5.53.tunarr.com"}; !slices.Equal(hide, want) {
		t.Fatalf("got %q, want %q", hide, want)
	}
	if got := hide.String(); got != "99,98,C5.53.tunarr.com" {
		t.Fatalf("String() = %q", got)
	}
}
