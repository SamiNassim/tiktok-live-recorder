package tiktok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

var (
	ErrNotFound    = errors.New("TikTok account was not found or has never hosted a live")
	ErrAccess      = errors.New("TikTok requires a valid account or permission to watch this live")
	ErrUnavailable = errors.New("streamer is live, but TikTok returned no supported FLV stream")
	ErrBlocked     = errors.New("TikTok returned an unexpected response; access may be temporarily limited")
	ErrRateLimited = errors.New("TikTok rate limited this connection; checks will back off")
)

type Source struct {
	URL, Quality, Codec    string
	Width, Height, Bitrate int
}

type Stream struct {
	Live                 bool
	RoomID, Name, Avatar string
	Sources              []Source
}

type Account struct{ ID, Name, Username, Avatar string }

// Client owns one paced API session. Media requests use a separate transport
// and never send account cookies to a CDN.
type Client struct {
	http          *http.Client
	base, webcast string
	mu            sync.Mutex
	cookie        string
	delay         time.Duration
	next          time.Time
	blockedUntil  time.Time
}

func NewClient() *Client {
	return &Client{http: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || req.URL.Scheme != "https" || req.URL.Hostname() != "www.tiktok.com" && req.URL.Hostname() != "webcast.tiktok.com" {
			return errors.New("unexpected TikTok API redirect")
		}
		return nil
	}}, base: Origin, webcast: "https://webcast.tiktok.com", delay: 2 * time.Second}
}

func (c *Client) Configure(cookie string, delay time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cookie, c.delay = cookie, delay
}

func (c *Client) get(ctx context.Context, address string, target any) error {
	c.mu.Lock()
	if time.Now().Before(c.blockedUntil) {
		c.mu.Unlock()
		return ErrRateLimited
	}
	at := maxTime(time.Now(), c.next)
	c.next = at.Add(c.delay)
	cookie := c.cookie
	c.mu.Unlock()
	t := time.NewTimer(time.Until(at))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return ErrBlocked
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", Origin+"/")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("could not reach TikTok; check the network and retry")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrAccess
	case http.StatusTooManyRequests:
		c.mu.Lock()
		c.blockedUntil = time.Now().Add(5 * time.Minute)
		c.mu.Unlock()
		return ErrRateLimited
	case http.StatusOK:
	default:
		return fmt.Errorf("TikTok returned HTTP %d", resp.StatusCode)
	}
	const limit = 4 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(body) > limit || json.Unmarshal(body, target) != nil {
		return ErrBlocked
	}
	return nil
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (c *Client) Check(ctx context.Context, username string) (Stream, error) {
	id, err := ParseID(username)
	if err != nil {
		return Stream{}, err
	}
	params := url.Values{"aid": {"1988"}, "sourceType": {"54"}, "uniqueId": {id}}
	var web webResponse
	err = c.get(ctx, c.base+"/api-live/user/room/?"+params.Encode(), &web)
	var primary Stream
	if err == nil {
		primary, err = web.stream()
	}
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrRateLimited) || ctx.Err() != nil {
		return primary, err
	}
	// Some accounts expose different playback data through the webcast route.
	var info roomResponse
	params = url.Values{"aid": {"1988"}, "unique_id": {id}}
	secondErr := c.get(ctx, c.webcast+"/webcast/room/info_by_user/?"+params.Encode(), &info)
	if secondErr == nil {
		var secondary Stream
		secondary, secondErr = info.stream()
		if secondErr == nil {
			return secondary, nil
		}
		if secondary.Live {
			primary = secondary
		}
	}
	if errors.Is(secondErr, ErrRateLimited) {
		return primary, secondErr
	}
	if primary.Live && !errors.Is(err, ErrAccess) && !errors.Is(secondErr, ErrAccess) {
		return primary, ErrUnavailable
	}
	return primary, err
}

// Account checks the server-side session, not merely the presence of a cookie.
func (c *Client) Account(ctx context.Context) (Account, error) {
	c.mu.Lock()
	cookie := c.cookie
	c.mu.Unlock()
	if cookie == "" {
		return Account{}, ErrAccess
	}
	var response struct {
		Message string `json:"message"`
		Data    struct {
			ID        string `json:"user_id_str"`
			Username  string `json:"username"`
			Name      string `json:"screen_name"`
			Avatar    string `json:"avatar_url"`
			ErrorCode int    `json:"error_code"`
		} `json:"data"`
	}
	if err := c.get(ctx, c.base+"/passport/web/account/info/?aid=1988&app_language=en&app_name=tiktok_web", &response); err != nil {
		return Account{}, err
	}
	if response.Message != "success" || response.Data.ErrorCode != 0 || response.Data.ID == "" || response.Data.Username == "" {
		return Account{}, errors.New("TikTok session is expired or could not be verified; sign in again and copy a fresh Cookie header")
	}
	name := response.Data.Name
	if name == "" {
		name = response.Data.Username
	}
	return Account{response.Data.ID, name, response.Data.Username, response.Data.Avatar}, nil
}
