package app

import (
	"fmt"
	"net"
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

// GitOfflineHint is shown when the API answers but git (fetch over ssh/https) does not: syncs work, new code
// cannot be fetched.
const GitOfflineHint = "git недоступен — новый код не скачается"

type reachState struct {
	mu         sync.Mutex
	checked    time.Time
	offline    string // the API does not answer: why ("" = it does)
	gitOffline string // the git endpoint of origin does not answer: why ("" = it does, or it is the API host itself)
	checking   bool
	dial       func(addr string, timeout time.Duration) error // replaced in tests
}

func tcpDial(addr string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// gitLabEndpoints are the host:port of the API and of git (from the origin remote; "" when it is the API one or
// a local path).
func (s *Service) gitLabEndpoints() (api, git string) {
	host, _ := s.DefaultProject()
	api = gitlab.APIEndpoint(host)
	if s.Settings.ProjectRoot != "" {
		if ep, ok := gitlab.GitEndpoint(gitlab.OriginURL(s.Settings.ProjectRoot)); ok && ep != api {
			git = ep
		}
	}
	return api, git
}

// CheckGitLab dials the GitLab endpoints now (cached for reachTTL) and returns "" when the API answers, otherwise
// why not: that is what syncs need. Whether git answers is kept for the pages (GitOffline). With
// GITLAB_REACH_CHECK=0 everything is considered reachable.
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
	api, git := s.gitLabEndpoints()
	probe := func(ep string) string {
		if ep == "" {
			return ""
		}
		if err := dial(ep, reachDialTimeout); err != nil {
			return fmt.Sprintf("%s: %v", ep, err)
		}
		return ""
	}
	offline, gitOffline := probe(api), probe(git)
	s.reach.mu.Lock()
	s.reach.checked, s.reach.offline, s.reach.gitOffline = time.Now(), offline, gitOffline
	s.reach.mu.Unlock()
	return offline
}

// GitLabOffline is the last known reachability of the API for pages ("" = reachable or not checked yet). It
// never blocks: when the result is stale a check starts in the background and the next page shows it.
func (s *Service) GitLabOffline() string {
	offline, _ := s.reachability()
	return offline
}

// GitOffline is the last known reachability of git for pages ("" = reachable, not checked, or the API is down —
// then GitLabOffline says it all).
func (s *Service) GitOffline() string {
	offline, git := s.reachability()
	if offline != "" {
		return ""
	}
	return git
}

func (s *Service) reachability() (api, git string) {
	if !s.Settings.GitLabReachCheck {
		return "", ""
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
	return s.reach.offline, s.reach.gitOffline
}
