// Package usage polls the subscription usage of the coding agents (Claude Code, Codex) so the dashboard can show how
// much of the 5-hour / weekly windows is spent before starting another run. It only reads the credentials the CLIs
// keep on this machine and never refreshes or stores them.
package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Limit is one usage window of an agent.
type Limit struct {
	Kind     string    `json:"kind"`             // session | weekly | model | extra | window
	Label    string    `json:"label"`            // "5 ч", "неделя", "Fable", "доп. кредиты"
	Percent  float64   `json:"percent"`          // 0..100 used
	Severity string    `json:"severity"`         // normal | warning | critical
	ResetsAt time.Time `json:"resets_at"`        // zero when unknown
	Model    string    `json:"model,omitempty"`  // model-scoped limits: the model's display name
	Detail   string    `json:"detail,omitempty"` // extra text for the tooltip (credits spent, …)
	Active   bool      `json:"active,omitempty"` // the provider marks this window as the binding one
}

// Snapshot is the last known usage of one agent.
type Snapshot struct {
	Runner    string    `json:"runner"`
	Plan      string    `json:"plan,omitempty"`
	Limits    []Limit   `json:"limits"`
	FetchedAt time.Time `json:"fetched_at"`
	Error     string    `json:"error,omitempty"`
}

// Fetcher loads the usage of one agent.
type Fetcher func(ctx context.Context) (Snapshot, error)

// Poller fetches every agent's usage on an interval and keeps the last snapshots.
type Poller struct {
	Interval time.Duration
	Client   *http.Client
	// Credential files and endpoints; empty = the CLI defaults (overridable for tests).
	ClaudeCredentials string
	ClaudeURL         string
	CodexAuth         string
	CodexURL          string

	mu    sync.Mutex
	snaps map[string]Snapshot
}

// New builds a poller with the CLI defaults: ~/.claude/.credentials.json (CLAUDE_CONFIG_DIR honoured) and
// ~/.codex/auth.json (CODEX_HOME honoured).
func New(interval time.Duration) *Poller {
	home, _ := os.UserHomeDir()
	claudeDir := firstNonEmpty(os.Getenv("CLAUDE_CONFIG_DIR"), filepath.Join(home, ".claude"))
	codexDir := firstNonEmpty(os.Getenv("CODEX_HOME"), filepath.Join(home, ".codex"))
	return &Poller{
		Interval:          interval,
		Client:            &http.Client{Timeout: 20 * time.Second},
		ClaudeCredentials: filepath.Join(claudeDir, ".credentials.json"),
		ClaudeURL:         "https://api.anthropic.com/api/oauth/usage",
		CodexAuth:         filepath.Join(codexDir, "auth.json"),
		CodexURL:          "https://chatgpt.com/backend-api/wham/usage",
		snaps:             map[string]Snapshot{},
	}
}

// Run refreshes right away and then every Interval until ctx is done. runners lists the agents to poll.
func (p *Poller) Run(ctx context.Context, runners []string) {
	p.Refresh(ctx, runners)
	interval := p.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.Refresh(ctx, runners)
		}
	}
}

// Refresh fetches the usage of the given runners once; failures are recorded in the snapshot, not returned.
func (p *Poller) Refresh(ctx context.Context, runners []string) {
	for _, name := range runners {
		var fetch Fetcher
		switch name {
		case "claude":
			fetch = p.fetchClaude
		case "codex":
			fetch = p.fetchCodex
		default:
			continue
		}
		snap, err := fetch(ctx)
		snap.Runner, snap.FetchedAt = name, time.Now()
		if err != nil {
			snap.Error = err.Error()
			// keep the last good limits so the chips do not blink on a transient error
			if prev, ok := p.get(name); ok && len(prev.Limits) > 0 && len(snap.Limits) == 0 {
				snap.Limits, snap.Plan = prev.Limits, prev.Plan
			}
		}
		p.mu.Lock()
		if p.snaps == nil {
			p.snaps = map[string]Snapshot{}
		}
		p.snaps[name] = snap
		p.mu.Unlock()
	}
}

func (p *Poller) get(name string) (Snapshot, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.snaps[name]
	return s, ok
}

// Snapshots returns a copy of the last snapshots keyed by runner name.
func (p *Poller) Snapshots() map[string]Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]Snapshot, len(p.snaps))
	for k, v := range p.snaps {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------------- Claude Code

type claudeCredentials struct {
	OAuth struct {
		AccessToken      string `json:"accessToken"`
		ExpiresAt        int64  `json:"expiresAt"`
		SubscriptionType string `json:"subscriptionType"`
		RateLimitTier    string `json:"rateLimitTier"`
	} `json:"claudeAiOauth"`
}

type claudeUsage struct {
	Limits []struct {
		Kind     string  `json:"kind"`
		Group    string  `json:"group"`
		Percent  float64 `json:"percent"`
		Severity string  `json:"severity"`
		ResetsAt string  `json:"resets_at"`
		IsActive bool    `json:"is_active"`
		Scope    *struct {
			Model *struct {
				DisplayName string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
	} `json:"limits"`
	Extra *struct {
		Enabled       bool    `json:"is_enabled"`
		MonthlyLimit  float64 `json:"monthly_limit"`
		UsedCredits   float64 `json:"used_credits"`
		Utilization   float64 `json:"utilization"`
		Currency      string  `json:"currency"`
		DecimalPlaces int     `json:"decimal_places"`
	} `json:"extra_usage"`
}

// fetchClaude reads the Claude Code OAuth token and asks the usage endpoint Claude Code itself uses for /usage.
func (p *Poller) fetchClaude(ctx context.Context) (Snapshot, error) {
	var snap Snapshot
	raw, err := os.ReadFile(p.ClaudeCredentials)
	if err != nil {
		return snap, fmt.Errorf("учётные данные Claude Code не найдены (%s): войдите в claude на этой машине", p.ClaudeCredentials)
	}
	var creds claudeCredentials
	if json.Unmarshal(raw, &creds) != nil || creds.OAuth.AccessToken == "" {
		return snap, errors.New("в файле учётных данных Claude Code нет OAuth-токена (вход по API-ключу usage не показывает)")
	}
	snap.Plan = strings.TrimSpace(strings.Join(nonEmpty(creds.OAuth.SubscriptionType, strings.TrimPrefix(creds.OAuth.RateLimitTier, "default_")), " · "))
	if creds.OAuth.ExpiresAt > 0 && time.UnixMilli(creds.OAuth.ExpiresAt).Before(time.Now()) {
		return snap, errors.New("токен Claude Code истёк: любой запуск claude обновит его")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.ClaudeURL, nil)
	req.Header.Set("Authorization", "Bearer "+creds.OAuth.AccessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	body, err := p.do(req)
	if err != nil {
		return snap, fmt.Errorf("usage Claude: %w", err)
	}
	var u claudeUsage
	if err := json.Unmarshal(body, &u); err != nil {
		return snap, fmt.Errorf("usage Claude: неожиданный ответ: %w", err)
	}
	for _, l := range u.Limits {
		lim := Limit{Percent: l.Percent, Severity: severity(l.Percent, l.Severity), ResetsAt: parseTime(l.ResetsAt), Active: l.IsActive}
		switch {
		case l.Kind == "session":
			lim.Kind, lim.Label = "session", "5 ч"
		case l.Scope != nil && l.Scope.Model != nil && l.Scope.Model.DisplayName != "":
			lim.Kind, lim.Label, lim.Model = "model", l.Scope.Model.DisplayName, l.Scope.Model.DisplayName
		case l.Group == "weekly":
			lim.Kind, lim.Label = "weekly", "неделя"
		default:
			lim.Kind, lim.Label = "window", l.Kind
		}
		snap.Limits = append(snap.Limits, lim)
	}
	if u.Extra != nil && u.Extra.Enabled && u.Extra.MonthlyLimit > 0 {
		div := 1.0
		for i := 0; i < u.Extra.DecimalPlaces; i++ {
			div *= 10
		}
		snap.Limits = append(snap.Limits, Limit{Kind: "extra", Label: "доп. кредиты", Percent: u.Extra.Utilization, Severity: severity(u.Extra.Utilization, ""),
			Detail: fmt.Sprintf("%.2f из %.2f %s за месяц", u.Extra.UsedCredits/div, u.Extra.MonthlyLimit/div, u.Extra.Currency)})
	}
	sortLimits(snap.Limits)
	return snap, nil
}

// ---------------------------------------------------------------------------------- Codex

type codexAuth struct {
	Tokens struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

type codexUsage struct {
	PlanType  string `json:"plan_type"`
	RateLimit struct {
		Primary   *codexWindow `json:"primary_window"`
		Secondary *codexWindow `json:"secondary_window"`
	} `json:"rate_limit"`
	Credits *struct {
		HasCredits bool   `json:"has_credits"`
		Balance    string `json:"balance"`
	} `json:"credits"`
}

type codexWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowSeconds int64   `json:"limit_window_seconds"`
	ResetAt       int64   `json:"reset_at"`
}

// fetchCodex reads the Codex CLI login and asks the usage endpoint behind its /status screen.
func (p *Poller) fetchCodex(ctx context.Context) (Snapshot, error) {
	var snap Snapshot
	raw, err := os.ReadFile(p.CodexAuth)
	if err != nil {
		return snap, fmt.Errorf("учётные данные Codex не найдены (%s): выполните codex login", p.CodexAuth)
	}
	var auth codexAuth
	if json.Unmarshal(raw, &auth) != nil || auth.Tokens.AccessToken == "" {
		return snap, errors.New("в auth.json Codex нет токена ChatGPT (вход по API-ключу usage не показывает)")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.CodexURL, nil)
	req.Header.Set("Authorization", "Bearer "+auth.Tokens.AccessToken)
	if auth.Tokens.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", auth.Tokens.AccountID)
	}
	req.Header.Set("Accept", "application/json")
	body, err := p.do(req)
	if err != nil {
		return snap, fmt.Errorf("usage Codex: %w", err)
	}
	var u codexUsage
	if err := json.Unmarshal(body, &u); err != nil {
		return snap, fmt.Errorf("usage Codex: неожиданный ответ: %w", err)
	}
	snap.Plan = u.PlanType
	for _, w := range []struct {
		win  *codexWindow
		kind string
	}{{u.RateLimit.Primary, "session"}, {u.RateLimit.Secondary, "weekly"}} {
		if w.win == nil {
			continue
		}
		lim := Limit{Kind: w.kind, Label: windowLabel(w.win.WindowSeconds), Percent: w.win.UsedPercent, Severity: severity(w.win.UsedPercent, "")}
		if w.win.ResetAt > 0 {
			lim.ResetsAt = time.Unix(w.win.ResetAt, 0)
		}
		snap.Limits = append(snap.Limits, lim)
	}
	if u.Credits != nil && u.Credits.HasCredits && u.Credits.Balance != "" {
		snap.Limits = append(snap.Limits, Limit{Kind: "extra", Label: "кредиты", Detail: "баланс " + u.Credits.Balance})
	}
	return snap, nil
}

// ---------------------------------------------------------------------------------- helpers

func (p *Poller) do(req *http.Request) ([]byte, error) {
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("HTTP %d — токен не принят, перелогиньтесь в CLI агента", resp.StatusCode)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, nil
}

// severity keeps the provider's word when given, else derives it from the percentage.
func severity(percent float64, provided string) string {
	switch strings.ToLower(provided) {
	case "critical", "warning", "normal":
		return strings.ToLower(provided)
	}
	switch {
	case percent >= 90:
		return "critical"
	case percent >= 70:
		return "warning"
	}
	return "normal"
}

func windowLabel(seconds int64) string {
	switch {
	case seconds <= 0:
		return "окно"
	case seconds%(7*24*3600) == 0:
		return "неделя"
	case seconds%(24*3600) == 0:
		return fmt.Sprintf("%d д", seconds/(24*3600))
	default:
		return fmt.Sprintf("%d ч", (seconds+1799)/3600)
	}
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// sortLimits orders: session, weekly, model-scoped, extra.
func sortLimits(limits []Limit) {
	rank := map[string]int{"session": 0, "weekly": 1, "model": 2, "window": 3, "extra": 4}
	sort.SliceStable(limits, func(i, j int) bool { return rank[limits[i].Kind] < rank[limits[j].Kind] })
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func nonEmpty(values ...string) []string {
	var out []string
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}
