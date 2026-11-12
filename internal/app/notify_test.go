package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"mr-review/internal/db"
	"mr-review/internal/notify"
	"mr-review/internal/testutil"
)

// Telegram: a finished run and a permission prompt are reported with a link; TELEGRAM_NO_TITLES drops titles.
func TestTelegramRunNotifications(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		texts = append(texts, body["text"].(string))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	got := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), texts...) }

	svc, _, fr := newService(t)
	svc.Settings.DashboardURL = "http://dash.lan:8765"
	svc.telegram = notify.New("tok", "chat")
	svc.telegram.API = srv.URL
	mr, _ := svc.AddMR("!42")
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	id, _ := svc.StartReview(mr.ID, db.KindReviewFull, "", 0)
	testutil.WaitFor(t, func() bool { return len(got()) == 1 })
	if msg := got()[0]; !strings.Contains(msg, "✓ Готово: Полное ревью !42 «MR 42»") || !strings.Contains(msg, "http://dash.lan:8765/run/"+itoa(id)) {
		t.Fatalf("%q", msg)
	}

	svc.Settings.TelegramNoTitles = true
	fr.AskWrite("/tmp/x.txt")
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	id, _ = svc.StartReview(mr.ID, db.KindReviewQuick, "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, id) == db.StatusWaiting && len(got()) == 2 })
	if msg := got()[1]; !strings.Contains(msg, "⚠ Агент ждёт разрешения: Быстрое ревью !42\n") || strings.Contains(msg, "MR 42") || !strings.Contains(msg, "Write") {
		t.Fatalf("%q", msg)
	}
	pending, _ := svc.DB.PendingApproval(id)
	_ = svc.Decide(pending.ID, "deny", "")
	testutil.WaitFor(t, func() bool { return status(svc, id) == db.StatusDone })
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
