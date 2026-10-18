package main

import (
	"errors"
	"net"
	"net/url"
	"strconv"
)

// Normalize the address once so the transport and health probe use the same port.
func normalizeProxy(raw string) (string, string, error) {
	if raw == "" {
		return "", "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", "", errors.New("proxy must include a valid scheme and host")
	}
	port := u.Port()
	switch u.Scheme {
	case "http":
		if port == "" {
			port = "80"
		}
	case "https":
		if port == "" {
			port = "443"
		}
	case "socks5", "socks5h":
		if port == "" {
			return "", "", errors.New("SOCKS proxy requires an explicit port")
		}
	default:
		return "", "", errors.New("unsupported proxy scheme")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", "", errors.New("proxy port must be between 1 and 65535")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", "", errors.New("proxy URL must not contain a path, query or fragment")
	}
	u.Host = net.JoinHostPort(u.Hostname(), port)
	return u.String(), u.Host, nil
}
