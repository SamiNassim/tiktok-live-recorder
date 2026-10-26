// Package tiktok resolves live media through TikTok's web endpoints.
package tiktok

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

const Origin = "https://www.tiktok.com"
const UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.]{0,23}$`)

// ParseID accepts usernames and canonical profile/live URLs, not unrelated links.
func ParseID(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if strings.Contains(s, "://") || strings.HasPrefix(s, "www.") || strings.HasPrefix(s, "tiktok.com/") {
		if !strings.Contains(s, "://") {
			s = "https://" + s
		}
		u, err := url.Parse(s)
		if err != nil || u.User != nil || u.Port() != "" || u.Scheme != "https" && u.Scheme != "http" || !strings.EqualFold(u.Hostname(), "www.tiktok.com") && !strings.EqualFold(u.Hostname(), "tiktok.com") {
			return "", errors.New("enter a TikTok username or a tiktok.com/@username profile URL")
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) == 0 || !strings.HasPrefix(parts[0], "@") || len(parts) > 2 || len(parts) == 2 && parts[1] != "live" {
			return "", errors.New("use a TikTok profile or live URL, not a video or share link")
		}
		s = parts[0]
	}
	s = strings.TrimPrefix(s, "@")
	if !usernamePattern.MatchString(s) || strings.HasSuffix(s, ".") || strings.Contains(s, "..") {
		return "", errors.New("TikTok usernames contain up to 24 letters, numbers, underscores, or periods")
	}
	return strings.ToLower(s), nil
}

// ParseCookie accepts a sessionid value, Cookie header, or JSON object. Extra
// routing cookies are preserved when supplied, but are not needed for web login.
func ParseCookie(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) > 64<<10 || strings.ContainsAny(raw, "\r\n\x00") {
		return "", errors.New("paste the sessionid value without line breaks")
	}
	if !strings.ContainsAny(raw, "=;{ \t") {
		if len(raw) != 32 || strings.Trim(raw, "0123456789abcdefABCDEF") != "" {
			return "", errors.New("paste the 32-character sessionid cookie value from TikTok")
		}
		raw = "sessionid=" + raw
	}
	if strings.HasPrefix(raw, "{") {
		var values map[string]string
		if err := json.Unmarshal([]byte(raw), &values); err != nil {
			return "", errors.New("cookie JSON must contain cookie names and string values")
		}
		var request http.Request
		request.Header = make(http.Header)
		for name, value := range values {
			cookie := &http.Cookie{Name: name, Value: value}
			if err := cookie.Valid(); err != nil {
				return "", errors.New("invalid cookie name or value")
			}
			request.AddCookie(cookie)
		}
		raw = request.Header.Get("Cookie")
	}
	if len(raw) >= 7 && strings.EqualFold(raw[:7], "cookie:") {
		raw = strings.TrimSpace(raw[7:])
	}
	req := &http.Request{Header: http.Header{"Cookie": []string{raw}}}
	cookies := req.Cookies()
	values := make(map[string]string)
	for _, cookie := range cookies {
		if err := cookie.Valid(); err != nil {
			return "", errors.New("invalid cookie value")
		}
		values[cookie.Name] = cookie.Value
	}
	if values["sessionid"] == "" && values["sessionid_ss"] == "" && values["sid_tt"] == "" {
		return "", errors.New("paste the sessionid cookie value from a signed-in TikTok account")
	}
	// Canonical ordering makes validation and configuration comparisons stable.
	var names []string
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	var out http.Request
	out.Header = make(http.Header)
	for _, name := range names {
		out.AddCookie(&http.Cookie{Name: name, Value: values[name]})
	}
	return out.Header.Get("Cookie"), nil
}
