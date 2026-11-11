package app

import (
	"net"
	"strings"
	"testing"
	"time"

	"mr-review/internal/gitlab"
	"mr-review/internal/testutil"
)

// closedPort returns a local address nobody listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestCheckGitLabClosedPort(t *testing.T) {
	settings, _ := testutil.Settings(t)
	settings.GitLabReachCheck = true
	settings.GitLabHost = closedPort(t)
	svc, gl, _ := newServiceWith(t, settings)

	start := time.Now()
	offline := svc.CheckGitLab()
	if offline == "" || !strings.Contains(offline, settings.GitLabHost) {
		t.Fatalf("closed port must be reported: %q", offline)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("check took %s", time.Since(start))
	}
	// The background sync does not call GitLab while it is unreachable and says why.
	svc.autoSyncOnce()
	if _, msg := svc.AutoSyncState(); !strings.HasPrefix(msg, OfflineHint) {
		t.Fatalf("sync state: %q", msg)
	}
	for _, call := range gl.Calls {
		if strings.HasPrefix(call, "list") || strings.HasPrefix(call, "issues") {
			t.Fatalf("no sync calls while offline: %v", gl.Calls)
		}
	}
	// Pages read the cached state without dialing.
	if svc.GitLabOffline() == "" {
		t.Fatal("cached offline state expected")
	}
}

func TestCheckGitLabOpenPortAndCache(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	settings, _ := testutil.Settings(t)
	settings.GitLabReachCheck = true
	settings.GitLabHost = l.Addr().String()
	svc, _, _ := newServiceWith(t, settings)
	dials := 0
	svc.reach.dial = func(addr string, timeout time.Duration) error { dials++; return tcpDial(addr, timeout) }
	if off := svc.CheckGitLab(); off != "" {
		t.Fatalf("open port must be reachable: %q", off)
	}
	svc.CheckGitLab()
	if dials != 1 {
		t.Fatalf("result must be cached for %s: %d dials", reachTTL, dials)
	}
	// Turned off: always reachable, nothing dialed.
	svc.Settings.GitLabReachCheck = false
	svc.reach.checked = time.Time{}
	if svc.CheckGitLab() != "" || svc.GitLabOffline() != "" || dials != 1 {
		t.Fatal("GITLAB_REACH_CHECK=0 must skip the check")
	}
}

func TestGitEndpoint(t *testing.T) {
	cases := map[string]string{
		"ssh://git@gitlab.example.com:22122/group/p.git": "gitlab.example.com:22122",
		"git@gitlab.example.com:group/p.git":             "gitlab.example.com:22",
		"https://gitlab.example.com/group/p.git":         "gitlab.example.com:443",
		"/home/me/repo.git":                              "",
		"file:///home/me/repo.git":                       "",
	}
	for remote, want := range cases {
		if got, _ := gitlab.GitEndpoint(remote); got != want {
			t.Errorf("%s: got %q want %q", remote, got, want)
		}
	}
	if gitlab.APIEndpoint("gitlab.example.com") != "gitlab.example.com:443" || gitlab.APIEndpoint("h:8443") != "h:8443" {
		t.Fatal("api endpoint")
	}
}
