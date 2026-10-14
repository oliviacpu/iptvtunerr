package plexlabelproxy

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func socketProxy(t *testing.T) *Proxy {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)
	proxy, err := New(Config{Upstream: up.URL, Token: "label-token", Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	return proxy
}

func waitForSocket(t *testing.T, path string) os.FileInfo {
	t.Helper()
	// Wait until it accepts connections: a stale socket file can exist first
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", path); err == nil {
			_ = conn.Close()
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat %s: %v", path, err)
			}
			return fi
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("socket %s never appeared", path)
	return nil
}

func TestProxy_ServesOnAPrivateUnixSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "proxy.sock")
	// A socket left behind by a previous run is replaced
	stale, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()

	proxy := socketProxy(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- proxy.ListenAndServe(ctx, "unix:"+sock) }()

	if fi := waitForSocket(t, sock); fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %o, want 600", fi.Mode().Perm())
	}
	client := http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	resp, err := client.Get("http://plex/library/sections")
	if err != nil {
		t.Fatalf("request over the socket: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d over the socket", resp.StatusCode)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket still there after shutdown: %v", err)
	}
}

func TestProxy_RefusesToReplaceAFileThatIsNotASocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.sock")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := socketProxy(t).ListenAndServe(ctx, "unix:"+path); err == nil {
		t.Fatal("listening over a regular file should fail")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "keep me" {
		t.Errorf("the file was touched: %q %v", data, err)
	}
}

func TestApparentSource_ForwardedForTrustedOnlyFromSocketOrLoopback(t *testing.T) {
	forwarded := func(remote string, socket bool) string {
		req := httptest.NewRequest(http.MethodGet, "/livetv/dvrs", nil)
		req.RemoteAddr = remote
		req.Header.Set("X-Forwarded-For", "203.0.113.7")
		if socket {
			req = req.WithContext(context.WithValue(req.Context(), privateSocketPeerKey{}, true))
		}
		return apparentSource(req)
	}
	if got := forwarded("", true); got != "203.0.113.7" {
		t.Errorf("Unix socket peer: source %q, want the forwarded address", got)
	}
	if got := forwarded("127.0.0.1:5000", false); got != "203.0.113.7" {
		t.Errorf("loopback peer: source %q, want the forwarded address", got)
	}
	if got := forwarded("198.51.100.1:5000", false); got != "198.51.100.1" {
		t.Errorf("remote peer: source %q, want its own address", got)
	}
}
