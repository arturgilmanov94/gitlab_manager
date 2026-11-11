package gitlab

import (
	"net"
	"net/url"
	"os/exec"
	"strings"
)

// GitEndpoint is the host:port git talks to for a remote URL: ssh:// and scp-like remotes use port 22 unless
// given, https 443, http 80. Local paths and file:// remotes have no network endpoint ("", false).
func GitEndpoint(remote string) (string, bool) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", false
	}
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil || u.Hostname() == "" {
			return "", false
		}
		port := u.Port()
		switch {
		case port != "":
		case u.Scheme == "ssh" || u.Scheme == "git+ssh":
			port = "22"
		case u.Scheme == "https":
			port = "443"
		case u.Scheme == "http":
			port = "80"
		case u.Scheme == "git":
			port = "9418"
		default:
			return "", false
		}
		return net.JoinHostPort(u.Hostname(), port), true
	}
	if m := remoteSCP.FindStringSubmatch(remote); m != nil && m[1] != "" {
		return net.JoinHostPort(m[1], "22"), true
	}
	return "", false
}

// APIEndpoint is the host:port of the GitLab API for a host name as configured (port 443 unless one is given).
func APIEndpoint(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(host, "443")
}

// OriginURL returns the URL of the origin remote of the repository at root ("" when there is none).
func OriginURL(root string) string {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
