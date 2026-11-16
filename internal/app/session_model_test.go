package app

import (
	"testing"

	"mr-review/internal/db"
)

func TestSessionModel(t *testing.T) {
	cases := []struct{ used, requested, want string }{
		{"claude-opus-5-5, claude-haiku-5-5", "opus", "claude-opus-5-5"},
		{"claude-haiku-5-5, claude-opus-5-5", "opus", "claude-opus-5-5"},
		{"", "opus", "opus"},
		{"claude-fable-5-2, claude-haiku-5-5", "", "claude-fable-5-2"},
		{"claude-fable-5-2", "default", "claude-fable-5-2"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := sessionModel(&db.Run{Model: c.used, RequestedModel: c.requested}); got != c.want {
			t.Errorf("used=%q requested=%q: got %q, want %q", c.used, c.requested, got, c.want)
		}
	}
}
