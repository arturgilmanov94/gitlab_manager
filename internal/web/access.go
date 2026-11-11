package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"rsc.io/qr"

	"mr-review/internal/config"
)

// Access from other devices (a phone on the same Wi-Fi or VPN). This machine is always let in; another device
// needs the access key once — from the link or QR code on «С телефона» — and then keeps it in a cookie.

const accessCookie = "mrr_access"

// loadAccessKey returns the key other devices need, or "" when the server only listens on this machine. Without
// ACCESS_KEY a random key is generated once and kept next to the database (readable by the owner only).
func loadAccessKey(s *config.Settings) (string, error) {
	if config.IsLoopbackHost(s.Host) {
		return "", nil
	}
	if s.AccessKey != "" {
		return s.AccessKey, nil
	}
	path := s.AccessKeyFile()
	if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) != "" {
		return strings.TrimSpace(string(data)), nil
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	key := base64.RawURLEncoding.EncodeToString(raw)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("cannot store the access key in %s: %w", path, err)
	}
	return key, nil
}

// isLocalRequest reports whether the request comes from this machine.
func isLocalRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// guard lets this machine through and asks any other device for the access key.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.accessKey == "" || isLocalRequest(r) || strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/favicon.ico" {
			next.ServeHTTP(w, r)
			return
		}
		if given := r.URL.Query().Get("key"); given != "" {
			if !s.keyMatches(given) {
				s.denied(w, r, "Ключ в ссылке не подходит — откройте свежую ссылку или QR-код со страницы «С телефона» на компьютере.")
				return
			}
			http.SetCookie(w, &http.Cookie{Name: accessCookie, Value: given, Path: "/", MaxAge: 365 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode})
			clean := *r.URL
			q := clean.Query()
			q.Del("key")
			clean.RawQuery = q.Encode()
			http.Redirect(w, r, clean.RequestURI(), http.StatusSeeOther)
			return
		}
		if c, err := r.Cookie(accessCookie); err == nil && s.keyMatches(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		s.denied(w, r, "Это устройство ещё не подключено. На компьютере откройте dashboard → «С телефона» и отсканируйте QR-код или откройте ссылку оттуда.")
	})
}

func (s *Server) keyMatches(given string) bool {
	return subtle.ConstantTimeCompare([]byte(given), []byte(s.accessKey)) == 1
}

func (s *Server) denied(w http.ResponseWriter, r *http.Request, message string) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": message})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	fmt.Fprintf(w, `<!doctype html><html lang="ru"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>MR Review · нужен ключ</title><link rel="stylesheet" href="/static/style.css"></head>
<body><main class="container"><div class="card" style="max-width:520px;margin:40px auto"><h1>MR Review</h1><p>%s</p></div></main></body></html>`, template.HTMLEscapeString(message))
}

// PhoneLink is one address of this machine another device can open, with its QR code.
type PhoneLink struct {
	Label, Iface, URL string
	QR                template.HTML
}

// phoneLinks lists the URLs (with the key) on every network this machine is on: Wi-Fi first, then VPN and wired;
// container bridges are skipped.
func (s *Server) phoneLinks() []PhoneLink {
	ifaces, _ := net.Interfaces()
	var out []PhoneLink
	for _, iface := range ifaces {
		name := iface.Name
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || strings.HasPrefix(name, "docker") ||
			strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "virbr") {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			link := "http://" + net.JoinHostPort(ipnet.IP.String(), strconv.Itoa(s.svc.Settings.Port)) + "/?key=" + url.QueryEscape(s.accessKey)
			out = append(out, PhoneLink{Label: ifaceLabel(name), Iface: name, URL: link, QR: qrSVG(link)})
		}
	}
	rank := map[string]int{"Wi-Fi": 0, "Сеть": 1, "VPN": 2}
	sort.SliceStable(out, func(i, j int) bool {
		ri, ok := rank[out[i].Label]
		if !ok {
			ri = 3
		}
		rj, ok := rank[out[j].Label]
		if !ok {
			rj = 3
		}
		return ri < rj
	})
	return out
}

func ifaceLabel(name string) string {
	switch {
	case strings.HasPrefix(name, "wl"):
		return "Wi-Fi"
	case strings.HasPrefix(name, "tun"), strings.HasPrefix(name, "tap"), strings.HasPrefix(name, "wg"), strings.HasPrefix(name, "ppp"):
		return "VPN"
	case strings.HasPrefix(name, "en"), strings.HasPrefix(name, "eth"):
		return "Сеть"
	}
	return name
}

// qrSVG draws a QR code of text as inline SVG ("" when the text does not fit).
func qrSVG(text string) template.HTML {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return ""
	}
	const quiet = 4
	size := code.Size + 2*quiet
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="qr" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges"><rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, size, size, size, size)
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			if code.Black(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return template.HTML(b.String())
}

func (s *Server) phonePage(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"Base": s.base("phone", "С телефона"), "Enabled": s.accessKey != "", "Local": isLocalRequest(r),
		"Port": s.svc.Settings.Port, "EnvFile": s.svc.Settings.EnvFile()}
	if s.accessKey != "" && isLocalRequest(r) {
		data["Links"] = s.phoneLinks()
	}
	s.render(w, "phone", data)
}
