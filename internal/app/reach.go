package app

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"mr-review/internal/gitlab"
)

// Reachability of GitLab: a TCP dial to the git host (origin remote) and the API host. It tells "VPN is off"
// apart from real errors in seconds, before a sync or a run waits for glab or git to give up.
const (
	reachDialTimeout = 3 * time.Second
	reachTTL         = 30 * time.Second
)

// OfflineHint is the short message shown wherever GitLab is unreachable.
const OfflineHint = "GitLab недоступен — VPN?"

type reachState struct {
	mu       sync.Mutex
	checked  time.Time
	offline  string // "" = reachable; otherwise which endpoint failed and why
	checking bool
	dial     func(addr string, timeout time.Duration) error // replaced in tests
}

func tcpDial(addr string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// gitLabEndpoints are the host:port pairs that must answer: git (from the origin remote) and the API.
func (s *Service) gitLabEndpoints() []string {
	var out []string
	if s.Settings.ProjectRoot != "" {
		if ep, ok := gitlab.GitEndpoint(gitlab.OriginURL(s.Settings.ProjectRoot)); ok {
			out = append(out, ep)
		}
	}
	host, _ := s.DefaultProject()
	if ep := gitlab.APIEndpoint(host); ep != "" && !containsStr(out, ep) {
		out = append(out, ep)
	}
	return out
}

// CheckGitLab dials the GitLab endpoints now (cached for reachTTL) and returns "" when they answer, otherwise
// why not. With GITLAB_REACH_CHECK=0 GitLab is always considered reachable.
func (s *Service) CheckGitLab() string {
	if !s.Settings.GitLabReachCheck {
		return ""
	}
	s.reach.mu.Lock()
	if !s.reach.checked.IsZero() && time.Since(s.reach.checked) < reachTTL {
		defer s.reach.mu.Unlock()
		return s.reach.offline
	}
	dial := s.reach.dial
	s.reach.mu.Unlock()
	if dial == nil {
		dial = tcpDial
	}
	var failed []string
	for _, ep := range s.gitLabEndpoints() {
		if err := dial(ep, reachDialTimeout); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", ep, err))
		}
	}
	offline := strings.Join(failed, "; ")
	s.reach.mu.Lock()
	s.reach.checked, s.reach.offline = time.Now(), offline
	s.reach.mu.Unlock()
	return offline
}

// GitLabOffline is the last known reachability for pages ("" = reachable or not checked yet). It never blocks:
// when the result is stale a check starts in the background and the next page shows it.
func (s *Service) GitLabOffline() string {
	if !s.Settings.GitLabReachCheck {
		return ""
	}
	s.reach.mu.Lock()
	defer s.reach.mu.Unlock()
	if (s.reach.checked.IsZero() || time.Since(s.reach.checked) >= reachTTL) && !s.reach.checking {
		s.reach.checking = true
		go func() {
			s.CheckGitLab()
			s.reach.mu.Lock()
			s.reach.checking = false
			s.reach.mu.Unlock()
		}()
	}
	return s.reach.offline
}
