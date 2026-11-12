package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTelegram records sendMessage calls; fail makes it answer like a wrong token.
type fakeTelegram struct {
	mu    sync.Mutex
	texts []string
	paths []string
	fail  bool
}

func (f *fakeTelegram) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.texts = append(f.texts, body["text"].(string))
		f.paths = append(f.paths, r.URL.Path)
		fail := f.fail
		f.mu.Unlock()
		if fail {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"ok":false,"description":"Unauthorized"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTelegramSendDedupeAndRateLimit(t *testing.T) {
	if New("", "1").Enabled() || New("tok", "").Enabled() {
		t.Fatal("without token or chat id notifications are off")
	}
	if sent, err := New("", "").Notify("k", "x"); sent || err != nil {
		t.Fatal("a disabled notifier does nothing")
	}
	fake := &fakeTelegram{}
	tg := New("secret-token", "42")
	tg.API = fake.server(t).URL
	clock := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	tg.now = func() time.Time { return clock }

	if sent, err := tg.Notify("run 1 done", "✓ Готово"); !sent || err != nil {
		t.Fatalf("%v %v", sent, err)
	}
	if sent, _ := tg.Notify("run 1 done", "✓ Готово"); sent {
		t.Fatal("the same event is not repeated within 10 minutes")
	}
	clock = clock.Add(11 * time.Minute)
	if sent, _ := tg.Notify("run 1 done", "✓ Готово"); !sent {
		t.Fatal("after the pause it may be sent again")
	}
	for i := 0; i < 20; i++ {
		tg.Notify("", "spam")
	}
	if n := len(fake.texts); n != perMinute+1 {
		t.Fatalf("rate limit: %d messages", n)
	}
	if fake.paths[0] != "/botsecret-token/sendMessage" {
		t.Fatal(fake.paths[0])
	}
	fake.fail = true
	if err := tg.Send("x"); err == nil || strings.Contains(err.Error(), "secret-token") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("errors never show the token: %v", err)
	}
}
