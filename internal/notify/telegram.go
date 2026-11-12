// Package notify sends short notifications about agent runs to Telegram (G7). Without a bot token and a chat id
// it is off and every call is a no-op.
package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Telegram posts messages through the Bot API (sendMessage).
type Telegram struct {
	Token  string
	ChatID string
	API    string // Bot API base, "" = https://api.telegram.org (tests point it to httptest)
	Client *http.Client

	mu     sync.Mutex
	recent map[string]time.Time // dedupe key → when it was last sent
	window []time.Time          // send times within the last minute (rate limit)
	now    func() time.Time
}

// Limits against spam: the same event is not repeated within dedupeFor, and at most perMinute messages go out.
const (
	dedupeFor = 10 * time.Minute
	perMinute = 12
)

// ErrDisabled is returned by Send when no token / chat id is configured.
var ErrDisabled = errors.New("Telegram не настроен: задайте TELEGRAM_BOT_TOKEN и TELEGRAM_CHAT_ID в .env")

// New returns a notifier; it is disabled unless both token and chat id are set.
func New(token, chatID string) *Telegram {
	return &Telegram{Token: strings.TrimSpace(token), ChatID: strings.TrimSpace(chatID), Client: &http.Client{Timeout: 10 * time.Second}}
}

// Enabled reports whether messages are sent at all.
func (t *Telegram) Enabled() bool { return t != nil && t.Token != "" && t.ChatID != "" }

// Notify sends text unless the same key was sent recently or the rate limit is reached; it reports whether the
// message went out. key identifies the event (e.g. "run 42 failed"); "" disables the dedupe.
func (t *Telegram) Notify(key, text string) (bool, error) {
	if !t.Enabled() {
		return false, nil
	}
	if !t.admit(key) {
		return false, nil
	}
	return true, t.Send(text)
}

// admit applies the dedupe and the rate limit.
func (t *Telegram) admit(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.now != nil {
		now = t.now()
	}
	if t.recent == nil {
		t.recent = map[string]time.Time{}
	}
	if key != "" {
		if at, ok := t.recent[key]; ok && now.Sub(at) < dedupeFor {
			return false
		}
	}
	kept := t.window[:0]
	for _, at := range t.window {
		if now.Sub(at) < time.Minute {
			kept = append(kept, at)
		}
	}
	t.window = kept
	if len(t.window) >= perMinute {
		return false
	}
	t.window = append(t.window, now)
	if key != "" {
		t.recent[key] = now
	}
	return true
}

// Send posts one message without the dedupe and the rate limit (the «Тестовое сообщение» button).
func (t *Telegram) Send(text string) error {
	if !t.Enabled() {
		return ErrDisabled
	}
	api := strings.TrimRight(t.API, "/")
	if api == "" {
		api = "https://api.telegram.org"
	}
	body, _ := json.Marshal(map[string]any{"chat_id": t.ChatID, "text": text, "disable_web_page_preview": true})
	resp, err := t.Client.Post(api+"/bot"+t.Token+"/sendMessage", "application/json", bytes.NewReader(body))
	if err != nil {
		// The URL holds the token: never put the raw error (it quotes the URL) into logs or the UI.
		return errors.New("Telegram недоступен: " + redact(err.Error(), t.Token))
	}
	defer resp.Body.Close()
	var answer struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&answer)
	if resp.StatusCode != http.StatusOK || !answer.OK {
		return fmt.Errorf("Telegram ответил %d: %s", resp.StatusCode, redact(answer.Description, t.Token))
	}
	return nil
}

func redact(text, secret string) string {
	if secret == "" {
		return text
	}
	return strings.ReplaceAll(text, secret, "***")
}
