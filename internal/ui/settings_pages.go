package ui

import (
	"fmt"
	"net"
	"os"
	"strings"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/browser"
	"reproxy/internal/config"
)

// ---- Traffic Proxy -----------------------------------------------------------------

// newProxyPage builds the Traffic Proxy section: where the proxy listens,
// and the CA it decrypts HTTPS with.
func newProxyPage(ctrl Controller) *settingsPage {
	body := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(body)
	v.SetContentsMargins(0, 0, 0, 0)
	v.SetSpacing(10)

	v.AddWidget(sectionTitle("Listen address"))
	form := newSettingsForm()
	host := qt.NewQComboBox2()
	host.SetEditable(true)
	host.AddItems(listenHostChoices())
	host.SetMinimumWidth(240)
	port := qt.NewQSpinBox2()
	port.SetRange(1, 65535)
	port.SetButtonSymbols(qt.QAbstractSpinBox__NoButtons) // typed, not stepped
	port.SetMinimumWidth(120)
	form.AddRow3("Host / interface", host.QWidget)
	form.AddRow3("Port", port.QWidget)
	v.AddLayout(form.QLayout)

	hint := helpText("")
	warning := qt.NewQLabel2()
	setName(warning.QObject, "WarningText")
	warning.SetWordWrap(true)
	v.AddWidget(hint.QWidget)
	v.AddWidget(warning.QWidget)

	update := func() {
		h := strings.TrimSpace(host.CurrentText())
		addr := net.JoinHostPort(h, fmt.Sprint(port.Value()))
		hint.SetText(fmt.Sprintf("Point your browser's SOCKS5 proxy at %s. "+
			"127.0.0.1 keeps the proxy private to this machine; 0.0.0.0 listens on every interface.", addr))
		ip := net.ParseIP(h)
		warning.SetVisible(ip != nil && !ip.IsLoopback())
		warning.SetText(fmt.Sprintf("The proxy has no authentication. Anyone who can reach %s "+
			"can use it to send requests from this machine.", addr))
	}
	host.OnCurrentTextChanged(func(string) { update() })
	port.OnValueChanged(func(int) { update() })

	// HTTPS interception CA.
	v.AddSpacing(10)
	v.AddWidget(sectionTitle("HTTPS interception"))
	v.AddWidget(helpText("To decrypt HTTPS, the proxy signs certificates with its own CA. " +
		"A browser must trust that CA, or it will warn on every site. The embedded browser " +
		"(the Browser button, top right) trusts it automatically; for any other browser, download the " +
		"certificate below and add it to the browser's or system's trusted authorities.").QWidget)

	caRow := qt.NewQHBoxLayout2()
	download := qt.NewQPushButton3("Download CA certificate…")
	download.SetAutoDefault(false)
	download.OnClicked(func() { saveCACert(body, ctrl) })
	caRow.AddWidget(download.QWidget)
	caRow.AddStretch()
	v.AddLayout(caRow.QLayout)

	fp := helpText("SHA-256: " + ctrl.CAFingerprint())
	fp.SetTextInteractionFlags(qt.TextSelectableByMouse)
	v.AddWidget(fp.QWidget)

	return &settingsPage{
		title:       settingsProxy,
		description: "Where the SOCKS5 proxy listens, and the CA it uses to decrypt HTTPS.",
		body:        body,
		load: func(c config.Config) {
			host.SetCurrentText(c.ProxyHost)
			port.SetValue(c.ProxyPort)
			update()
		},
		store: func(c *config.Config) {
			c.ProxyHost = strings.TrimSpace(host.CurrentText())
			c.ProxyPort = port.Value()
		},
	}
}

// ---- Outbound Traffic -------------------------------------------------------

// newOutboundPage builds the Outbound Traffic section: the upstream proxies
// connections leave reProxy through. It returns the upstream editor as
// well, because the upstream proxies' passwords are saved to the secrets
// file rather than through config.Config (see settings_upstream.go).
func newOutboundPage(ctrl Controller) (*settingsPage, *upstreamEditor) {
	body := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(body)
	v.SetContentsMargins(0, 0, 0, 0)
	v.SetSpacing(10)

	v.AddWidget(sectionTitle("Upstream Proxy Configuration"))
	override := newOverrideNote()
	v.AddWidget(override.QWidget)
	up := newUpstreamEditor(ctrl)
	v.AddWidget(up.root)

	return &settingsPage{
		title: settingsOutbound,
		description: "Where connections go after they leave reProxy: the browser's traffic, " +
			"replays, agents' and plugins' requests, and the DNS lookups they need.",
		body:  body,
		load:  func(c config.Config) { up.load(c.Upstream) },
		store: func(c *config.Config) { c.Upstream = up.chain() },
		shown: func() {
			e, name, ok := ctrl.EngagementSettings()
			override.show(ok && e.Upstream != nil, "The open engagement, "+name+", uses its own upstream "+
				"chain instead of this one while it is open (Settings → Engagement → Outbound Traffic).")
		},
	}, up
}

// listenHostChoices lists loopback, wildcard and this machine's own
// addresses. The combo box stays editable for anything else.
func listenHostChoices() []string {
	choices := []string{"127.0.0.1", "0.0.0.0"}
	seen := map[string]bool{"127.0.0.1": true, "0.0.0.0": true}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.IsLinkLocalUnicast() {
				continue
			}
			if s := ipnet.IP.String(); !seen[s] {
				seen[s] = true
				choices = append(choices, s)
			}
		}
	}
	for _, s := range []string{"::1", "::"} {
		if !seen[s] {
			choices = append(choices, s)
		}
	}
	return choices
}

func newSettingsForm() *qt.QFormLayout {
	form := qt.NewQFormLayout2()
	form.SetHorizontalSpacing(16)
	form.SetVerticalSpacing(10)
	form.SetLabelAlignment(qt.AlignLeft | qt.AlignVCenter)
	form.SetFieldGrowthPolicy(qt.QFormLayout__FieldsStayAtSizeHint)
	return form
}

// ---- Interception ----------------------------------------------------------

// option pairs a combo box label with the value it stands for.
type option[T comparable] struct {
	label string
	value T
}

func fillCombo[T comparable](c *qt.QComboBox, opts []option[T]) {
	for _, o := range opts {
		c.AddItem(o.label)
	}
}

func selectOption[T comparable](c *qt.QComboBox, opts []option[T], v T) {
	for i, o := range opts {
		if o.value == v {
			c.SetCurrentIndex(i)
			return
		}
	}
	c.SetCurrentIndex(0)
}

func selectedOption[T comparable](c *qt.QComboBox, opts []option[T]) T {
	if i := c.CurrentIndex(); i >= 0 && i < len(opts) {
		return opts[i].value
	}
	return opts[0].value
}

// saveCACert prompts for a location and writes the proxy's CA certificate
// there, so it can be installed into another browser or the OS trust store.
func saveCACert(parent *qt.QWidget, ctrl Controller) {
	path := qt.QFileDialog_GetSaveFileName4(parent, "Save CA certificate",
		"reproxy-ca.pem", "Certificates (*.pem *.crt);;All files (*)")
	if path == "" {
		return
	}
	if err := os.WriteFile(path, ctrl.CACertPEM(), 0o644); err != nil {
		qt.QMessageBox_Warning(parent, "Couldn't save certificate", err.Error())
		return
	}
	qt.QMessageBox_Information(parent, "Certificate saved",
		"Saved the CA certificate to:\n"+path+
			"\n\nInstall it as a trusted certificate authority in the browser you're testing with.")
}

// ---- Browser ---------------------------------------------------------------

func newBrowserPage(installed []browser.Browser) *settingsPage {
	body := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(body)
	v.SetContentsMargins(0, 0, 0, 0)
	v.SetSpacing(10)

	v.AddWidget(sectionTitle("Browser"))
	form := newSettingsForm()
	choice := qt.NewQComboBox2()
	auto := "Automatic"
	if len(installed) > 0 {
		auto += " (" + installed[0].Name + ")"
	}
	choice.AddItem(auto)
	for _, b := range installed {
		choice.AddItem(b.Name)
	}
	choice.SetMinimumWidth(240)
	custom := qt.NewQLineEdit2()
	custom.SetPlaceholderText("Path to a browser executable")
	custom.SetMinimumWidth(360)
	form.AddRow3("Use", choice.QWidget)
	form.AddRow3("Or this executable", custom.QWidget)
	v.AddLayout(form.QLayout)
	syncCustom := func() { choice.SetEnabled(strings.TrimSpace(custom.Text()) == "") }
	custom.OnTextChanged(func(string) { syncCustom() })

	found := "No supported browser was found on this machine. Install Chrome, Chromium, Edge, " +
		"Brave or Firefox, or give the path to one above."
	if len(installed) > 0 {
		var names []string
		for _, b := range installed {
			names = append(names, b.Name)
		}
		found = "Found: " + strings.Join(names, ", ") + "."
	}
	v.AddWidget(helpText(found).QWidget)
	v.AddWidget(helpText("The Browser button at the top right (Ctrl+Shift+B) starts the browser with its own profile, " +
		"already pointed at the proxy, so your everyday browser and its settings are never touched. " +
		"The profile is kept between sessions in the browser folder of the data directory. Browsers " +
		"opened this way are closed when reProxy exits.").QWidget)

	return &settingsPage{
		title:       settingsBrowser,
		description: "The browser launched from the Browser button, preconfigured to use the proxy.",
		body:        body,
		load: func(c config.Config) {
			choice.SetCurrentIndex(0)
			custom.SetText("")
			for i, b := range installed {
				if b.Path == c.Browser.Path {
					choice.SetCurrentIndex(i + 1)
					syncCustom()
					return
				}
			}
			custom.SetText(c.Browser.Path)
			syncCustom()
		},
		store: func(c *config.Config) {
			c.Browser.Path = strings.TrimSpace(custom.Text())
			if i := choice.CurrentIndex(); c.Browser.Path == "" && i > 0 {
				c.Browser.Path = installed[i-1].Path
			}
		},
	}
}
