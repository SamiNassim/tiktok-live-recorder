package app

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"tiktok-live-recorder/internal/tiktok"
)

type sessionBadge struct {
	name, detail, tooltip string
	avatar                string
	attention, invalid    bool
}

// Describe only the saved session, never an unsaved replacement or an account
// returned for another cookie. Guest access stays available for public streams.
func (a *application) sessionBadge() sessionBadge {
	saved, savedErr := tiktok.ParseCookie(a.store.Config().Cookie)
	cookie, err := tiktok.ParseCookie(a.cfg.Cookie)
	if err != nil || savedErr != nil {
		return sessionBadge{name: "Session invalid", detail: "Check in Settings", tooltip: "The cookie format is invalid. Update it in Settings.", invalid: true}
	}
	if cookie != saved {
		return sessionBadge{name: "Session changes unsaved", detail: "Save in Settings", tooltip: "Save your session changes in Settings before this account is used.", attention: true}
	}
	if saved == "" {
		return sessionBadge{name: "Guest mode", detail: "Account optional · Settings", tooltip: "Public streams can be recorded without an account. Set up an optional TikTok account in Settings for restricted streams."}
	}
	if a.account != nil && a.accountCookie == saved && a.account.Username != "" {
		username := "@" + strings.TrimPrefix(strings.TrimSpace(a.account.Username), "@")
		name := strings.TrimSpace(a.account.Name)
		if name == "" {
			name = username
		}
		return sessionBadge{name: name, detail: username, avatar: a.account.Avatar, tooltip: name + "\n" + username + "\nLast verified account for the saved session. Open Settings to validate again."}
	}
	if a.validating || a.cookieLastChecked != a.cfg.Cookie {
		return sessionBadge{name: "Checking session…", detail: "Verifying TikTok account", tooltip: "Checking the account associated with your saved session.", attention: true}
	}
	if errors.Is(a.accountError, tiktok.ErrSessionExpired) {
		return sessionBadge{name: "Session expired", detail: "Update in Settings", tooltip: "Sign in again and update your Cookie header in Settings.", invalid: true}
	}
	if errors.Is(a.accountError, tiktok.ErrAccess) {
		return sessionBadge{name: "Session rejected", detail: "Check in Settings", tooltip: "TikTok rejected account access. Validate the session again in Settings.", invalid: true}
	}
	return sessionBadge{name: "Session unverified", detail: "Check in Settings", tooltip: "The saved session could not be verified. Check your connection or validate the account again in Settings.", attention: true}
}

func (a *application) sessionIndicator(c *ui.Context) {
	badge := a.sessionBadge()
	t := c.Theme()
	color := t.Text
	if badge.attention {
		color = t.Warning
	}
	if badge.invalid {
		color = t.Danger
	}
	if ui.ButtonBase(c).Label("TikTok session").FillWidth().MinWidth(0).Padding(0).Tooltip(badge.tooltip).Children(func() {
		ui.Row(c).FillWidth().MinWidth(0).Gap(10).AlignItems(ui.Center).Children(func() {
			if badge.avatar != "" {
				if avatar := a.avatars[badge.avatar]; avatar != nil {
					ui.Image(c, avatar).Label("TikTok account avatar").Size(32, 32).Shrink(0).Radius(16)
				} else {
					initial, _ := utf8.DecodeRuneInString(badge.name)
					ui.Box(c).Label("TikTok account avatar placeholder").Size(32, 32).Shrink(0).Radius(16).Background(t.Accent.Alpha(0.15)).Center().Children(func() {
						ui.Text(c, strings.ToUpper(string(initial))).FontWeight(600).TextColor(t.Accent)
					})
				}
			}
			ui.Column(c).Grow(1).MinWidth(0).Gap(4).Children(func() {
				ui.Text(c, badge.name).FontSize(12).FontWeight(600).TextColor(color).FillWidth().MinWidth(0).SingleLine().Tooltip(badge.tooltip)
				ui.Text(c, badge.detail).FontSize(10).TextColor(t.TextMuted).FillWidth().MinWidth(0).SingleLine().Tooltip(badge.tooltip)
			})
		})
	}).Clicked() {
		a.page = "settings"
		a.accountSettingsRequested = true
	}
}
