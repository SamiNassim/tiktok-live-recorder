// Package network configures explicit HTTP, HTTPS, and SOCKS5 proxy routing.
package network

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Config is comparable so settings edits and connection changes can be detected.
// Credentials are kept separate from the address and never included in errors.
type Config struct {
	Enabled  bool   `json:"enabled,omitempty"`
	URL      string `json:"url,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

func (c Config) proxyURL() (*url.URL, error) {
	if !c.Enabled {
		return nil, nil
	}
	u, err := url.Parse(strings.TrimSpace(c.URL))
	if err != nil || u == nil || u.Hostname() == "" {
		return nil, errors.New("enter a proxy URL, such as socks5://127.0.0.1:1080 or http://127.0.0.1:8080")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, errors.New("proxy URL must start with http://, https://, or socks5://")
	}
	if u.User != nil {
		return nil, errors.New("enter proxy credentials in the separate username and password fields")
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("proxy URL must contain only a scheme, host, and port")
	}
	if u.Port() == "" {
		return nil, errors.New("include the port in the proxy URL")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("proxy port must be between 1 and 65535")
	}
	if c.Password != "" && c.Username == "" {
		return nil, errors.New("enter a proxy username when using a password")
	}
	if (u.Scheme == "http" || u.Scheme == "https") && strings.Contains(c.Username, ":") {
		return nil, errors.New("HTTP proxy usernames cannot contain a colon")
	}
	if strings.ContainsAny(c.Username+c.Password, "\r\n\x00") {
		return nil, errors.New("proxy credentials cannot contain line breaks or null characters")
	}
	if strings.HasPrefix(u.Scheme, "socks5") && (len(c.Username) > 255 || len(c.Password) > 255) {
		return nil, errors.New("SOCKS5 username and password must each fit in 255 bytes")
	}
	return u, nil
}

func (c Config) Validate() error {
	_, err := c.proxyURL()
	return err
}

// NewTransport preserves environment proxies unless an explicit proxy is enabled.
// Explicit routes also cover loopback hosts and never fall back to direct access.
func NewTransport(c Config) (*http.Transport, error) {
	u, err := c.proxyURL()
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if u != nil {
		if c.Username != "" {
			u.User = url.UserPassword(c.Username, c.Password)
		}
		transport.Proxy = http.ProxyURL(u)
	}
	return transport, nil
}

// Protect keeps proxy addresses, credentials, and server diagnostics out of logs.
func Protect(t *http.Transport, c Config) http.RoundTripper {
	if !c.Enabled {
		return t
	}
	return protectedTransport{t}
}

type protectedTransport struct{ *http.Transport }

func (t protectedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := t.Transport.RoundTrip(r)
	if err != nil {
		return nil, proxyFailure{err}
	}
	return response, nil
}

type proxyFailure struct{ cause error }

func (e proxyFailure) Error() string {
	return "the configured proxy connection failed; check the proxy address, credentials, and routing"
}
func (e proxyFailure) Unwrap() error             { return e.cause }
func (e proxyFailure) DiagnosticDetails() string { return e.Error() }
