package app

import (
	"context"
	"time"
)

// syncStamp is the outcome of the last background sync, shown in the header.
type syncStamp struct {
	At    time.Time
	Error string
}

// AutoSyncState reports when the background sync last ran and its last error ("" = fine).
func (s *Service) AutoSyncState() (time.Time, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSync.At, s.lastSync.Error
}

// RunAutoSync refreshes MRs and issues from GitLab every interval until ctx is done, so the lists stay current
// without pressing «Синхронизировать». The first pass runs right away. Open pages notice the change through
// DataVersion and refresh themselves.
func (s *Service) RunAutoSync(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.autoSyncOnce()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) autoSyncOnce() {
	if host, _ := s.DefaultProject(); host == "" {
		return
	}
	msg := ""
	if _, err := s.SyncMRs(); err != nil {
		msg = "MR: " + err.Error()
	}
	if _, err := s.SyncIssues(); err != nil {
		if msg != "" {
			msg += "; "
		}
		msg += "задачи: " + err.Error()
	}
	s.mu.Lock()
	s.lastSync = syncStamp{At: time.Now(), Error: msg}
	s.mu.Unlock()
}

// DataVersion identifies the current state of what a page shows: from GitLab (MRs and issues as synced) and the
// runs. An open page reloads itself once it differs from the version the page was rendered with. mrID > 0 narrows
// it to one MR and its runs (the MR page), 0 covers everything (lists and the overview).
func (s *Service) DataVersion(mrID int64) string {
	return s.DB.DataFingerprint(mrID)
}
