package app

import (
	"fmt"
	"net"
	"sync"
	"time"

	"mr-review/internal/gitlab"
	"mr-review/internal/worktree"
)

// Reachability of GitLab: a TCP dial to the API host. It tells "VPN is off" apart from real errors in seconds,
// before a sync waits for glab to give up. The git endpoint (ssh) is not dialed on a timer — every such dial is a
// line in the server's auth.log; whether git answers is learned from real fetches (noteFetch).
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
	gitDown    string // host:port of git a fetch could not connect to ("" = the last fetch connected, or none ran)
	gitOffline string // why gitDown does not answer
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

// gitEndpoint is the host:port of git for the checkout at root (from its origin remote; "" when it is the API one
// or a local path).
func (s *Service) gitEndpoint(root string) string {
	if root == "" {
		return ""
	}
	host, _ := s.DefaultProject()
	if ep, ok := gitlab.GitEndpoint(gitlab.OriginURL(root)); ok && ep != gitlab.APIEndpoint(host) {
		return ep
	}
	return ""
}

func (s *Service) probe(ep string) string {
	s.reach.mu.Lock()
	dial := s.reach.dial
	s.reach.mu.Unlock()
	if dial == nil {
		dial = tcpDial
	}
	if err := dial(ep, reachDialTimeout); err != nil {
		return fmt.Sprintf("%s: %v", ep, err)
	}
	return ""
}

// CheckGitLab dials the API endpoint now (cached for reachTTL) and returns "" when it answers, otherwise why not:
// that is what syncs need. The git endpoint is dialed only while the last fetch could not connect to it, to notice
// that it is back. With GITLAB_REACH_CHECK=0 everything is considered reachable.
func (s *Service) CheckGitLab() string {
	if !s.Settings.GitLabReachCheck {
		return ""
	}
	s.reach.mu.Lock()
	if !s.reach.checked.IsZero() && time.Since(s.reach.checked) < reachTTL {
		defer s.reach.mu.Unlock()
		return s.reach.offline
	}
	gitDown := s.reach.gitDown
	s.reach.mu.Unlock()
	host, _ := s.DefaultProject()
	offline := s.probe(gitlab.APIEndpoint(host))
	gitOffline := ""
	if gitDown != "" {
		gitOffline = s.probe(gitDown)
	}
	s.reach.mu.Lock()
	s.reach.checked, s.reach.offline = time.Now(), offline
	if s.reach.gitDown == gitDown {
		s.reach.gitOffline = gitOffline
		if gitOffline == "" {
			s.reach.gitDown = ""
		}
	}
	s.reach.mu.Unlock()
	return offline
}

// noteFetch records what a real `git fetch` of the checkout at root says about the git endpoint: a fetch that
// worked means it answers; after a failed one the endpoint is dialed once to tell "no connection" from a refusal
// (unknown ref, access).
func (s *Service) noteFetch(root string, err error) {
	if !s.Settings.GitLabReachCheck {
		return
	}
	ep := s.gitEndpoint(root)
	if ep == "" {
		return
	}
	why := ""
	if err != nil {
		if why = s.probe(ep); why == "" {
			return // the port answers: the failure is not about connectivity and says nothing new
		}
	}
	s.reach.mu.Lock()
	defer s.reach.mu.Unlock()
	if why != "" {
		s.reach.gitDown, s.reach.gitOffline = ep, why
	} else if s.reach.gitDown == ep {
		s.reach.gitDown, s.reach.gitOffline = "", ""
	}
}

// watchFetches makes the fetches of m report to noteFetch.
func (s *Service) watchFetches(m *worktree.Manager) *worktree.Manager {
	root := s.root
	m.OnFetch = func(err error) { root.noteFetch(m.Root, err) }
	return m
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
