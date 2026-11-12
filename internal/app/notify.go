package app

import (
	"fmt"
	"net"
	"strings"

	"mr-review/internal/db"
)

// Telegram notifications (G7): the agent waits for a permission or for answers, a run is done or failed. Off
// without TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID. Messages carry a link to the run on this dashboard.

// notifyKinds are the human names of run kinds in messages.
var notifyKinds = map[string]string{
	db.KindReviewQuick: "Быстрое ревью", db.KindReviewFull: "Полное ревью", db.KindReviewVerify: "Проверка изменений",
	db.KindVerifyFinding: "Проверка замечания", db.KindFixComments: "Исправление замечаний", db.KindPlan: "Исследование",
	db.KindImplement: "Реализация", db.KindStandTest: "Проверка на стенде", db.KindCIAnalyze: "Разбор CI",
	db.KindCIFix: "Исправление CI", db.KindFixFindings: "Исправление выбранного", db.KindAsk: "Вопрос агенту",
}

// TelegramEnabled reports whether notifications are configured.
func (s *Service) TelegramEnabled() bool { return s.telegram.Enabled() }

// TestTelegram sends a test message (the button on the environment page).
func (s *Service) TestTelegram() error {
	err := s.telegram.Send("mr-review: тестовое сообщение. Уведомления работают.\n" + s.DashboardURL())
	if err != nil {
		return &UserError{err.Error()}
	}
	return nil
}

// DashboardURL is where links in notifications point: DASHBOARD_URL, else the LAN address with REMOTE_ACCESS
// (a phone in the same Wi-Fi / VPN opens it), else this machine.
func (s *Service) DashboardURL() string {
	if s.Settings.DashboardURL != "" {
		return s.Settings.DashboardURL
	}
	host := "127.0.0.1"
	if s.Settings.RemoteAccess {
		if ip := lanAddress(); ip != "" {
			host = ip
		}
	}
	return fmt.Sprintf("http://%s:%d", host, s.Settings.Port)
}

// lanAddress is the first private IPv4 address of this machine ("" = none).
func lanAddress() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil && ipnet.IP.IsPrivate() {
			return ipnet.IP.String()
		}
	}
	return ""
}

// notifyRun tells about an event of a run: "permission" (detail = the call), "questions", "done", "failed".
// It never blocks the run: the message is sent in the background.
func (s *Service) notifyRun(runID int64, event, detail string) {
	if !s.telegram.Enabled() {
		return
	}
	run, _ := s.DB.GetRun(runID)
	if run == nil {
		return
	}
	var head string
	switch event {
	case "permission":
		head = "⚠ Агент ждёт разрешения"
	case "questions":
		head = "❓ Агент задал вопросы и ждёт ответа"
	case "done":
		head = "✓ Готово"
	case "failed":
		head = "✕ Не удалось"
	default:
		return
	}
	lines := []string{head + ": " + firstOf(notifyKinds[run.Kind], run.Kind) + " " + s.objectLabel(run)}
	if detail != "" {
		lines = append(lines, detail)
	}
	if event == "failed" && run.Error != "" {
		lines = append(lines, firstLine(run.Error))
	}
	lines = append(lines, fmt.Sprintf("%s/run/%d", s.DashboardURL(), run.ID))
	key := fmt.Sprintf("run %d %s %s", run.ID, event, detail)
	go func() {
		if _, err := s.telegram.Notify(key, strings.Join(lines, "\n")); err != nil {
			fmt.Printf("telegram: %v\n", err)
		}
	}()
}

// objectLabel is "!42 «Title»" / "#7 «Title»" (without titles under TELEGRAM_NO_TITLES).
func (s *Service) objectLabel(run *db.Run) string {
	ref, title := "", ""
	if run.MRID != nil {
		if mr, _ := s.DB.GetMR(*run.MRID); mr != nil {
			ref, title = fmt.Sprintf("!%d", mr.IID), mr.Title
		}
	} else if run.IssueID != nil {
		if issue, _ := s.DB.GetIssue(*run.IssueID); issue != nil {
			ref, title = fmt.Sprintf("#%d", issue.IID), issue.Title
		}
	}
	if ref == "" {
		return fmt.Sprintf("(запуск #%d)", run.ID)
	}
	if s.Settings.TelegramNoTitles || title == "" {
		return ref
	}
	return ref + " «" + title + "»"
}

// notifyFinished reports the outcome of a run that has just stopped (deferred in execute).
func (s *Service) notifyFinished(runID int64) {
	run, _ := s.DB.GetRun(runID)
	if run == nil {
		return
	}
	switch {
	case run.Status == db.StatusDone:
		s.notifyRun(runID, "done", "")
	case run.Status == db.StatusFailed:
		s.notifyRun(runID, "failed", "")
	case run.Status == db.StatusWaiting:
		if pending, _ := s.DB.PendingQuestions(runID); len(pending) > 0 {
			s.notifyRun(runID, "questions", "")
		}
	}
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return line
}
