package app

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"tiktok-live-recorder/internal/logging"
	"tiktok-live-recorder/internal/state"
	"tiktok-live-recorder/internal/tiktok"
)

const testAccountCookie = "sessionid=synthetic; tt-target-idc=testdc"

func accountFixture(t *testing.T) *application {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log, err := logging.Open(store.Directory())
	if err != nil {
		t.Fatal(err)
	}
	a := newApplication(store, log)
	a.preview = true // No real account requests or monitoring in UI tests.
	t.Cleanup(func() { a.cancel(); log.Close() })
	return a
}

func TestSessionBadgeStates(t *testing.T) {
	viewer := &tiktok.Account{ID: "7000000000000000002", Name: "Demo Viewer", Username: "demo_viewer", Avatar: "https://cdn.example/avatar.png"}
	for _, test := range []struct {
		name, saved, draft, checked, accountCookie string
		account                                    *tiktok.Account
		err                                        error
		validating                                 bool
		want                                       string
	}{
		{name: "guest", want: "Guest mode"},
		{name: "unsaved", draft: testAccountCookie, want: "Session changes unsaved"},
		{name: "removed but unsaved", saved: testAccountCookie, want: "Session changes unsaved"},
		{name: "startup", saved: testAccountCookie, draft: testAccountCookie, want: "Checking session…"},
		{name: "validating", saved: testAccountCookie, draft: testAccountCookie, checked: testAccountCookie, validating: true, want: "Checking session…"},
		{name: "expired", saved: testAccountCookie, draft: testAccountCookie, checked: testAccountCookie, err: tiktok.ErrSessionExpired, want: "Session expired"},
		{name: "rejected", saved: testAccountCookie, draft: testAccountCookie, checked: testAccountCookie, err: tiktok.ErrAccess, want: "Session rejected"},
		{name: "temporary failure", saved: testAccountCookie, draft: testAccountCookie, checked: testAccountCookie, err: errors.New("temporary network failure"), want: "Session unverified"},
		{name: "bad format", saved: "invalid value", draft: "invalid value", want: "Session invalid"},
		{name: "verified", saved: testAccountCookie, draft: testAccountCookie, accountCookie: testAccountCookie, account: viewer, want: "Demo Viewer"},
		{name: "canonical header", saved: "Cookie: tt-target-idc=testdc; sessionid=synthetic", draft: testAccountCookie, accountCookie: testAccountCookie, account: viewer, want: "Demo Viewer"},
		{name: "no display name", saved: testAccountCookie, draft: testAccountCookie, accountCookie: testAccountCookie, account: &tiktok.Account{Username: "demo_viewer"}, want: "@demo_viewer"},
		{name: "stale identity", saved: testAccountCookie, draft: testAccountCookie, accountCookie: "sessionid=other-synthetic; tt-target-idc=testdc", account: viewer, want: "Checking session…"},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := accountFixture(t)
			cfg := a.store.Config()
			cfg.Cookie = test.saved
			if err := a.store.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			a.cfg.Cookie = test.draft
			a.cookieLastChecked = test.checked
			a.accountCookie = test.accountCookie
			a.account, a.accountError, a.validating = test.account, test.err, test.validating
			badge := a.sessionBadge()
			if badge.name != test.want {
				t.Fatalf("got %q, want %q", badge.name, test.want)
			}
			if strings.Contains(badge.tooltip, "synthetic") {
				t.Fatal("credentials exposed in session tooltip")
			}
			if badge.avatar != "" && badge.name != "Demo Viewer" {
				t.Fatal("avatar shown for an unverified or different account")
			}
		})
	}
}

func TestSessionShortcutAnchorsAccountSettings(t *testing.T) {
	for _, size := range [][2]int{{1050, 650}, {1400, 900}} {
		a := accountFixture(t)
		tt := ui.NewTester(a.view, size[0], size[1])
		if !tt.HasText("Guest mode") || !tt.HasText("An account in Settings is optional for public streams.") {
			t.Fatal("guest support is not explained", tt.Texts())
		}
		for range 2 {
			if err := tt.Click("TikTok session"); err != nil {
				t.Fatal(err)
			}
			viewport, ok := tt.Find("Settings sections")
			if !ok {
				t.Fatal("settings scroller missing")
			}
			heading, ok := tt.Find("TikTok account")
			if !ok || math.Abs(float64(heading.Y-viewport.Y-21)) > 1 {
				t.Fatalf("account heading not at the top: heading=%+v viewport=%+v", heading, viewport)
			}
			if a.accountSettingsRequested {
				t.Fatal("account anchor request was not consumed")
			}
			a.settingsScroll.Y = a.settingsScroll.MaxY
			tt.Frame()
			if a.settingsScroll.Y != a.settingsScroll.MaxY {
				t.Fatal("account anchor prevented scrolling away")
			}
		}
		for _, page := range []string{"Streamers", "Settings"} {
			if err := tt.Click(page); err != nil {
				t.Fatal(err)
			}
		}
		if a.settingsScroll.Y != a.settingsScroll.MaxY {
			t.Fatal("ordinary navigation reset the settings scroll position")
		}
	}
}

func TestSidebarAccountIdentityFitsWithCachedAvatar(t *testing.T) {
	for _, name := range []string{"Demo Viewer", strings.Repeat("Long username ", 20), strings.Repeat("界🎵", 50)} {
		a := accountFixture(t)
		a.cfg.Cookie = testAccountCookie
		if err := a.store.SaveConfig(a.cfg); err != nil {
			t.Fatal(err)
		}
		username := strings.Repeat("long_username", 10)
		address := "https://cdn.example/avatar.png"
		a.account = &tiktok.Account{Name: name, Username: username, Avatar: address}
		a.accountCookie = testAccountCookie
		tt := ui.NewTester(a.view, 1050, 650)
		if _, ok := tt.Find("TikTok account avatar placeholder"); !ok {
			t.Fatal("avatar has no loading placeholder")
		}
		a.avatars[address] = a.logo
		tt.Frame()
		avatar, ok := tt.Find("TikTok account avatar")
		if !ok || avatar.W != 32 || avatar.H != 32 || avatar.X < 0 || avatar.X+avatar.W > 220 {
			t.Fatal("avatar missing or escaped sidebar", avatar)
		}
		for _, label := range []string{strings.TrimSpace(name), "@" + username} {
			r, ok := tt.Find(label)
			if !ok || r.X < avatar.X+avatar.W || r.X+r.W > 220 || r.W > 130 {
				t.Fatal("account identity escaped sidebar or overlaps avatar", r)
			}
		}
		badge := a.sessionBadge()
		if !strings.Contains(badge.tooltip, strings.TrimSpace(name)) || !strings.Contains(badge.tooltip, username) {
			t.Fatal("full identity missing from tooltip")
		}
		if len(a.avatarLoading) != 0 {
			t.Fatal("rendering the sidebar initiated another image request")
		}
		if r, ok := tt.Find("v" + Version); !ok || r.Y+r.H > 650 {
			t.Fatal("sidebar footer overflowed compact window", r)
		}
		a.cookieChanged()
		tt.Frame()
		if a.account != nil || a.accountCookie != "" || a.accountError != nil || a.cookieLastChecked != "" {
			t.Fatal("old session state retained after cookie edit")
		}
		if _, ok := tt.Find("TikTok account avatar"); ok {
			t.Fatal("avatar retained after cookie edit")
		}
	}
}
