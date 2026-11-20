package app

import "github.com/egoist/mygo/ui"

func (a *application) proxySettings(c *ui.Context) {
	section(c, "Proxy", "Optional connection routing for TikTok authentication, discovery, thumbnails, and recordings.", func() {
		before := a.cfg.Proxy
		ui.Row(c).FillWidth().Gap(20).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Grow(1).Gap(4).Children(func() {
				ui.Text(c, "Use a proxy").FontWeight(500)
				ui.Text(c, "Saving a proxy change stops active recordings. New recordings use the saved connection.").FontSize(11).TextColor(c.Theme().TextMuted)
			})
			ui.Switch(c, &a.cfg.Proxy.Enabled).Label("Enable proxy connection")
		})
		if a.cfg.Proxy.Enabled {
			ui.TextInput(c, &a.cfg.Proxy.URL).FillWidth().Height(34).Label("Proxy URL").Placeholder("socks5://127.0.0.1:1080 or http://127.0.0.1:8080")
			ui.Row(c).FillWidth().Gap(10).Children(func() {
				ui.TextInput(c, &a.cfg.Proxy.Username).Grow(1).Height(34).Label("Proxy username").Placeholder("Username (optional)")
				ui.TextInput(c, &a.cfg.Proxy.Password).Password().Grow(1).Height(34).Label("Proxy password").Placeholder("Password (optional)")
			})
			ui.Text(c, "Supports HTTP, HTTPS, and SOCKS5. Use a fixed exit compatible with your TikTok session and region. Failed proxy connections never fall back to a direct connection.").FontSize(11).TextColor(c.Theme().TextMuted)
			ui.Text(c, "TikTok may still reject an exit IP or require a fresh session. Proxy credentials are saved locally with your settings.").FontSize(11).TextColor(c.Theme().TextMuted)
			if err := a.cfg.Proxy.Validate(); err != nil {
				ui.Text(c, err.Error()).FontSize(11).TextColor(c.Theme().Warning)
			}
		}
		if before != a.cfg.Proxy {
			a.cookieChanged()
		}
	})
}
