package usage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const claudeBody = `{"limits":[
 {"kind":"session","group":"session","percent":21,"severity":"normal","resets_at":"2026-09-14T16:19:59.882251+00:00","scope":null,"is_active":false},
 {"kind":"weekly_all","group":"weekly","percent":52,"severity":"normal","resets_at":"2026-09-17T12:59:59.882273+00:00","scope":null,"is_active":false},
 {"kind":"weekly_scoped","group":"weekly","percent":97,"severity":"critical","resets_at":"2026-09-17T12:59:59.882486+00:00","scope":{"model":{"id":null,"display_name":"Fable"},"surface":null},"is_active":true}],
 "extra_usage":{"is_enabled":true,"monthly_limit":100000,"used_credits":49450.0,"utilization":49.45,"currency":"EUR","decimal_places":2}}`

const codexBody = `{"plan_type":"plus","rate_limit":{"allowed":true,"primary_window":{"used_percent":29,"limit_window_seconds":18000,"reset_after_seconds":14058,"reset_at":1789404655},
 "secondary_window":{"used_percent":13,"limit_window_seconds":604800,"reset_after_seconds":582852,"reset_at":1789973449}},"credits":{"has_credits":false,"balance":"0"}}`

func newPoller(t *testing.T, claude, codex http.HandlerFunc) *Poller {
	t.Helper()
	dir := t.TempDir()
	cs := httptest.NewServer(claude)
	xs := httptest.NewServer(codex)
	t.Cleanup(cs.Close)
	t.Cleanup(xs.Close)
	expires := time.Now().Add(time.Hour).UnixMilli()
	_ = os.WriteFile(filepath.Join(dir, "creds.json"), []byte(`{"claudeAiOauth":{"accessToken":"tok-claude","expiresAt":`+strconvI(expires)+`,"subscriptionType":"team","rateLimitTier":"default_claude_max_5x"}}`), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"tokens":{"access_token":"tok-codex","account_id":"acc-1"}}`), 0o600)
	return &Poller{Interval: time.Minute, Client: cs.Client(), ClaudeCredentials: filepath.Join(dir, "creds.json"), ClaudeURL: cs.URL, CodexAuth: filepath.Join(dir, "auth.json"), CodexURL: xs.URL}
}

// The Claude usage endpoint is read with the CLI's OAuth token; windows, the model-scoped weekly limit and the extra
// credits become chips. Codex gives its two windows. Errors are kept per agent and do not drop the last good data.
func TestPollerSnapshots(t *testing.T) {
	var claudeAuth, codexAuth, codexAccount string
	p := newPoller(t, func(w http.ResponseWriter, r *http.Request) {
		claudeAuth = r.Header.Get("Authorization")
		if r.Header.Get("anthropic-beta") == "" {
			w.WriteHeader(400)
			return
		}
		_, _ = w.Write([]byte(claudeBody))
	}, func(w http.ResponseWriter, r *http.Request) {
		codexAuth, codexAccount = r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-Id")
		if strings.HasSuffix(r.URL.Path, "/gone") {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(codexBody))
	})
	p.Refresh(context.Background(), []string{"claude", "codex", "cursor"})
	snaps := p.Snapshots()
	c := snaps["claude"]
	if claudeAuth != "Bearer tok-claude" || c.Error != "" || c.Plan != "team · claude_max_5x" || len(c.Limits) != 4 {
		t.Fatalf("%+v (auth %q)", c, claudeAuth)
	}
	if c.Limits[0].Label != "5 ч" || c.Limits[0].Percent != 21 || c.Limits[1].Label != "неделя" || c.Limits[2].Model != "Fable" || c.Limits[2].Severity != "critical" || !c.Limits[2].Active {
		t.Fatalf("%+v", c.Limits)
	}
	if c.Limits[3].Kind != "extra" || c.Limits[3].Detail != "494.50 из 1000.00 EUR за месяц" || c.Limits[3].Percent != 49.45 {
		t.Fatalf("%+v", c.Limits[3])
	}
	if c.Limits[0].ResetsAt.IsZero() || c.Limits[0].ResetsAt.UTC().Hour() != 16 {
		t.Fatalf("resets_at must be parsed: %v", c.Limits[0].ResetsAt)
	}
	x := snaps["codex"]
	if codexAuth != "Bearer tok-codex" || codexAccount != "acc-1" || x.Error != "" || x.Plan != "plus" || len(x.Limits) != 2 {
		t.Fatalf("%+v", x)
	}
	if x.Limits[0].Label != "5 ч" || x.Limits[0].Percent != 29 || x.Limits[1].Label != "неделя" || x.Limits[1].Percent != 13 || x.Limits[0].ResetsAt.Unix() != 1789404655 {
		t.Fatalf("%+v", x.Limits)
	}
	if _, ok := snaps["cursor"]; ok {
		t.Fatal("unknown runners are skipped")
	}
	// A failing endpoint keeps the previous limits and records the error.
	p.CodexURL = p.CodexURL + "/gone"
	p.Refresh(context.Background(), []string{"codex"})
	x = p.Snapshots()["codex"]
	if x.Error == "" || len(x.Limits) != 2 {
		t.Fatalf("error must be recorded and old limits kept: %+v", x)
	}
	// No credentials at all: a readable hint, no limits.
	p.ClaudeCredentials = filepath.Join(t.TempDir(), "missing.json")
	p.snaps = map[string]Snapshot{}
	p.Refresh(context.Background(), []string{"claude"})
	if c = p.Snapshots()["claude"]; !strings.Contains(c.Error, "не найдены") || len(c.Limits) != 0 {
		t.Fatalf("%+v", c)
	}
}

func TestSeverityAndLabels(t *testing.T) {
	if severity(10, "") != "normal" || severity(75, "") != "warning" || severity(95, "") != "critical" || severity(10, "critical") != "critical" {
		t.Fatal("severity thresholds")
	}
	if windowLabel(18000) != "5 ч" || windowLabel(604800) != "неделя" || windowLabel(86400) != "1 д" || windowLabel(0) != "окно" {
		t.Fatal("window labels")
	}
}

func strconvI(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
