package ui

import (
	"fmt"
	"strings"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/randid"
	"reproxy/internal/upstream"
)

// upstreamKindOptions are the protocols offered for each hop, in the order
// they appear in the combo box.
var upstreamKindOptions = []option[upstream.Kind]{
	{"HTTP", upstream.HTTP},
	{"HTTPS", upstream.HTTPS},
	{"SOCKS4", upstream.SOCKS4},
	{"SOCKS4a", upstream.SOCKS4a},
	{"SOCKS5", upstream.SOCKS5},
}

// defaultUpstreamPort is what a new hop starts on: the port a local proxy
// most often listens on.
const defaultUpstreamPort = 8080

// Column geometry shared by the header and each row so they line up. The
// host, username and password share what the fixed columns leave over, in
// these proportions: the host is the field most often long enough to matter.
const (
	upColOrder   = 16
	upColEnabled = 24
	upColKind    = 92
	upColPort    = 68

	upStretchHost = 2
	upStretchUser = 1
	upStretchPass = 1

	upMinHost = 110
	upMinAuth = 72
)

// upstreamEditor is the Upstream Proxy Configuration subsection: an ordered
// list of proxies, each reached through the one above it.
//
// Passwords are not part of config.Config — they are kept in the secrets
// file — so the settings dialog reads them back with passwords() and saves
// them separately, the way agent API keys are handled.
type upstreamEditor struct {
	ctrl Controller

	root    *qt.QWidget
	enabled *qt.QCheckBox
	verify  *qt.QCheckBox
	list    *qt.QVBoxLayout
	header  *qt.QWidget
	empty   *qt.QLabel
	rows    []*upstreamRow
	// removed are the proxies taken out since load, whose passwords go.
	removed []string
}

type upstreamRow struct {
	w        *qt.QWidget
	id       string
	enabled  *qt.QCheckBox
	kind     *qt.QComboBox
	host     *qt.QLineEdit
	port     *qt.QSpinBox
	user     *qt.QLineEdit
	pass     *qt.QLineEdit
	up, down *iconButton
	order    *qt.QLabel
}

func (r *upstreamRow) widget() *qt.QWidget { return r.w }

func newUpstreamEditor(ctrl Controller) *upstreamEditor {
	e := &upstreamEditor{ctrl: ctrl, root: qt.NewQWidget2()}
	v := qt.NewQVBoxLayout(e.root)
	v.SetContentsMargins(0, 0, 0, 0)
	v.SetSpacing(8)

	e.enabled = qt.NewQCheckBox3("Send connections through these proxies")
	v.AddWidget(e.enabled.QWidget)
	v.AddWidget(helpText("The chain is traversed in order: the first proxy is dialled directly, each " +
		"proxy below it is reached through the one above, and the target server is reached through " +
		"the last. That is how several layers wrap one another, so a host that is only visible from " +
		"behind the innermost proxy — a staging network, say — can be reached from here. Use the " +
		"arrows to change the order.").QWidget)

	box := qt.NewQWidget2()
	setName(box.QObject, "RulesEditor")
	box.SetAttribute(qt.WA_StyledBackground)
	bv := qt.NewQVBoxLayout(box)
	bv.SetContentsMargins(10, 8, 10, 10)
	bv.SetSpacing(6)

	e.header = upstreamHeaderRow()
	bv.AddWidget(e.header)

	e.empty = helpText("No upstream proxies — connections go straight to the target.")
	bv.AddWidget(e.empty.QWidget)

	listBox := qt.NewQWidget2()
	e.list = qt.NewQVBoxLayout(listBox)
	e.list.SetContentsMargins(0, 0, 0, 0)
	e.list.SetSpacing(6)
	bv.AddWidget(listBox)

	add := qt.NewQPushButton3("+ Add proxy")
	add.SetAutoDefault(false) // Enter in the dialog means OK, not "add"
	add.OnClicked(func() {
		e.insertRow(len(e.rows), upstream.Proxy{
			ID:      newUpstreamID(),
			Enabled: true,
			Kind:    upstream.HTTP,
			Port:    defaultUpstreamPort,
		}, "")
		e.rows[len(e.rows)-1].host.SetFocus()
	})
	addRow := qt.NewQHBoxLayout2()
	addRow.AddWidget(add.QWidget)
	addRow.AddStretch()
	bv.AddLayout(addRow.QLayout)
	v.AddWidget(box)

	e.verify = qt.NewQCheckBox3("Verify the TLS certificate of HTTPS proxies")
	v.AddWidget(e.verify.QWidget)
	v.AddWidget(helpText("This is about the certificate an HTTPS proxy presents for itself, not the " +
		"target server's. Turn it off for a proxy with a self-signed certificate.").QWidget)
	v.AddWidget(helpText("SOCKS5, SOCKS4a and HTTP CONNECT pass the host name to the proxy, so it is " +
		"resolved there. Plain SOCKS4 carries only an IPv4 address, so reProxy has to resolve the " +
		"name on this machine first — pick SOCKS4a instead when the name only exists upstream. " +
		"Passwords are kept in secrets.json, readable only by you, not in the settings file.").QWidget)

	// Nothing here is disabled while the chain is off: a chain is usually
	// filled in before it is switched on, and the checkbox above already
	// says whether it is in use.
	return e
}

// upstreamHeaderRow labels the columns. It is built here rather than with
// headerRowFor because three of the columns share the leftover width
// instead of being fixed.
func upstreamHeaderRow() *qt.QWidget {
	w := qt.NewQWidget2()
	h := qt.NewQHBoxLayout(w)
	h.SetContentsMargins(0, 0, 0, 0)
	h.SetSpacing(8)
	for _, c := range []struct {
		text     string
		width    int
		stretch  int
		minWidth int
	}{
		{text: "#", width: upColOrder},
		{text: "On", width: upColEnabled},
		{text: "Type", width: upColKind},
		{text: "Host", stretch: upStretchHost, minWidth: upMinHost},
		{text: "Port", width: upColPort},
		{text: "Username", stretch: upStretchUser, minWidth: upMinAuth},
		{text: "Password", stretch: upStretchPass, minWidth: upMinAuth},
		{width: ruleColMove}, {width: ruleColMove}, {width: ruleColRemove},
	} {
		l := qt.NewQLabel3(c.text)
		setName(l.QObject, "RulesHeader")
		if c.stretch > 0 {
			l.SetMinimumWidth(c.minWidth)
			h.AddWidget2(l.QWidget, c.stretch)
			continue
		}
		l.SetFixedWidth(c.width)
		h.AddWidget(l.QWidget)
	}
	return w
}

func (e *upstreamEditor) load(c upstream.Chain) {
	for len(e.rows) > 0 {
		e.removeRow(e.rows[0])
	}
	e.removed = nil
	e.enabled.SetChecked(c.Enabled)
	e.verify.SetChecked(c.VerifyTLS)
	for _, p := range c.Proxies {
		e.insertRow(len(e.rows), p, e.ctrl.UpstreamPassword(p.ID))
	}
	e.refresh()
}

// loadCopy fills the editor with a copy of c to edit as a chain of its own,
// as an engagement's chain starts from the global one: every proxy gets a
// new ID, so its password is saved apart from the original's, and starts
// out as the original's.
func (e *upstreamEditor) loadCopy(c upstream.Chain) {
	for len(e.rows) > 0 {
		e.removeRow(e.rows[0])
	}
	e.removed = nil
	e.enabled.SetChecked(c.Enabled)
	e.verify.SetChecked(c.VerifyTLS)
	for _, p := range c.Proxies {
		pw := e.ctrl.UpstreamPassword(p.ID)
		p.ID = newUpstreamID()
		e.insertRow(len(e.rows), p, pw)
	}
	e.refresh()
}

// chain returns the configured chain, without the passwords.
func (e *upstreamEditor) chain() upstream.Chain {
	c := upstream.Chain{
		Enabled:   e.enabled.IsChecked(),
		VerifyTLS: e.verify.IsChecked(),
	}
	for _, r := range e.rows {
		c.Proxies = append(c.Proxies, upstream.Proxy{
			ID:       r.id,
			Enabled:  r.enabled.IsChecked(),
			Kind:     selectedOption(r.kind, upstreamKindOptions),
			Host:     strings.TrimSpace(r.host.Text()),
			Port:     r.port.Value(),
			Username: strings.TrimSpace(r.user.Text()),
		})
	}
	return c
}

// passwords returns every row's password by proxy ID. Rows with no password,
// and proxies removed since load, are included as empty strings so saving
// removes any password left over from before.
func (e *upstreamEditor) passwords() map[string]string {
	out := make(map[string]string, len(e.rows)+len(e.removed))
	for _, id := range e.removed {
		out[id] = ""
	}
	for _, r := range e.rows {
		out[r.id] = r.pass.Text()
	}
	return out
}

func (e *upstreamEditor) insertRow(index int, p upstream.Proxy, password string) {
	r := &upstreamRow{w: qt.NewQWidget2(), id: p.ID}
	if r.id == "" {
		r.id = newUpstreamID()
	}
	h := qt.NewQHBoxLayout(r.w)
	h.SetContentsMargins(0, 0, 0, 0)
	h.SetSpacing(8)

	r.order = qt.NewQLabel2()
	setName(r.order.QObject, "Muted")
	r.order.SetFixedWidth(upColOrder)

	r.enabled = qt.NewQCheckBox2()
	r.enabled.SetChecked(p.Enabled)
	r.enabled.SetFixedWidth(upColEnabled)
	r.enabled.SetToolTip("Include this proxy in the chain")

	r.kind = qt.NewQComboBox2()
	fillCombo(r.kind, upstreamKindOptions)
	selectOption(r.kind, upstreamKindOptions, p.Kind)
	r.kind.SetFixedWidth(upColKind)

	r.host = qt.NewQLineEdit3(p.Host)
	r.host.SetPlaceholderText("proxy.example.test")
	r.host.SetMinimumWidth(upMinHost)
	r.port = qt.NewQSpinBox2()
	r.port.SetRange(1, 65535)
	r.port.SetButtonSymbols(qt.QAbstractSpinBox__NoButtons) // typed, not stepped
	r.port.SetValue(p.Port)
	r.port.SetFixedWidth(upColPort)

	r.user = qt.NewQLineEdit3(p.Username)
	r.user.SetMinimumWidth(upMinAuth)
	r.pass = qt.NewQLineEdit3(password)
	r.pass.SetEchoMode(qt.QLineEdit__Password)
	r.pass.SetMinimumWidth(upMinAuth)

	r.up = newIconButton(iconUp, "Move this proxy earlier in the chain", func() { e.move(r, -1) })
	r.down = newIconButton(iconDown, "Move this proxy later in the chain", func() { e.move(r, 1) })
	remove := newIconButton(iconClose, "Remove this proxy", func() {
		e.removeRow(r)
		e.refresh()
	}).asDanger()

	// SOCKS4 has no password; its username is sent as the USERID field.
	syncAuth := func() {
		switch selectedOption(r.kind, upstreamKindOptions) {
		case upstream.SOCKS4, upstream.SOCKS4a:
			r.pass.SetEnabled(false)
			r.pass.SetToolTip("SOCKS4 has no password; only the username (its USERID field) is sent")
			r.user.SetToolTip("Sent as the SOCKS4 USERID")
		default:
			r.pass.SetEnabled(true)
			r.pass.SetToolTip("")
			r.user.SetToolTip("")
		}
	}
	syncAuth()
	r.kind.OnCurrentIndexChanged(func(int) { syncAuth() })

	h.AddWidget(r.order.QWidget)
	h.AddWidget(r.enabled.QWidget)
	h.AddWidget(r.kind.QWidget)
	h.AddWidget2(r.host.QWidget, upStretchHost)
	h.AddWidget(r.port.QWidget)
	h.AddWidget2(r.user.QWidget, upStretchUser)
	h.AddWidget2(r.pass.QWidget, upStretchPass)
	h.AddWidget(r.up.w)
	h.AddWidget(r.down.w)
	h.AddWidget(remove.w)

	e.list.InsertWidget(index, r.w)
	e.rows = append(e.rows, nil)
	copy(e.rows[index+1:], e.rows[index:])
	e.rows[index] = r
	e.refresh()
}

// move shifts a proxy by delta places in the chain.
func (e *upstreamEditor) move(r *upstreamRow, delta int) {
	from := -1
	for i, row := range e.rows {
		if row == r {
			from = i
			break
		}
	}
	to := from + delta
	if from < 0 || to < 0 || to >= len(e.rows) {
		return
	}
	e.rows[from], e.rows[to] = e.rows[to], e.rows[from]
	e.list.RemoveWidget(r.w)
	e.list.InsertWidget(to, r.w)
	e.refresh()
}

func (e *upstreamEditor) removeRow(r *upstreamRow) {
	var dropped bool
	if e.rows, dropped = dropRow(e.rows, r); dropped {
		// Its password goes with it: passwords() writes an empty string for
		// every id in removed, so applying the settings clears it.
		e.removed = append(e.removed, r.id)
	}
}

// refresh renumbers the rows and greys out the move buttons that would do
// nothing, so the order of the chain is self-explanatory.
func (e *upstreamEditor) refresh() {
	e.header.SetVisible(len(e.rows) > 0)
	e.empty.SetVisible(len(e.rows) == 0)
	for i, r := range e.rows {
		r.order.SetText(fmt.Sprint(i + 1))
		r.up.setEnabled(i > 0)
		r.down.setEnabled(i < len(e.rows)-1)
	}
}

// newUpstreamID returns a short random identifier for a proxy, which keys
// its password in the secrets file.
func newUpstreamID() string { return randid.Hex(6) }
