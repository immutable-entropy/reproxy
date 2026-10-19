package ui

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/agent"
	"reproxy/internal/config"
	"reproxy/internal/randid"
)

// settingsAgents is added to the settings dialog's page list.
const settingsAgents = "Agents"

// agentDraft is a profile being edited plus its (possibly just typed) key,
// so edits aren't saved until Apply.
type agentDraft struct {
	profile config.AgentProfile
	key     string
}

// agentsPage is the Settings → Agents page: a list of configured agents on
// the left, the selected agent's fields on the right, and the MCP server
// settings below.
type agentsPage struct {
	ctrl Controller

	drafts   []agentDraft
	selected int

	list   *qt.QListWidget
	editor *qt.QWidget
	empty  *qt.QWidget
	stack  *qt.QStackedWidget

	name    *qt.QLineEdit
	vendor  *qt.QComboBox
	model   *qt.QLineEdit
	baseURL *qt.QLineEdit
	key     *qt.QLineEdit
	keyRow  *qt.QWidget
	review  *qt.QCheckBox

	mcpPort   *qt.QSpinBox
	mcpPolicy *qt.QComboBox
	token     *qt.QLineEdit

	loading bool // suppresses field-change handlers while loading a draft
	// warned is set once the user has been shown what an agent can read of
	// the traffic, so the warning is asked once per dialog as well as once
	// per user (config.Config.AgentWarningSeen, which the page's store
	// writes only when the settings are applied: a dialog the user cancels
	// adds no agent, so it should ask again).
	warned bool
}

func newAgentsPage(ctrl Controller) (*settingsPage, *agentsPage) {
	p := &agentsPage{ctrl: ctrl, selected: -1}

	body := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(body)
	v.SetContentsMargins(0, 0, 0, 0)
	v.SetSpacing(10)

	v.AddWidget(sectionTitle("Agents"))
	v.AddWidget(helpText("Add an AI agent by choosing a vendor and entering its API key. " +
		"Agents appear in the Agents panel at the bottom of the window. Keys are kept in " +
		"secrets.json, readable only by you, not in the settings file.").QWidget)

	row := qt.NewQHBoxLayout2()
	row.SetSpacing(12)

	// Left: the list and its add/remove buttons.
	left := qt.NewQVBoxLayout2()
	left.SetSpacing(6)
	p.list = qt.NewQListWidget2()
	setName(p.list.QObject, "MasterList")
	p.list.SetFixedWidth(200)
	p.list.OnCurrentRowChanged(func(r int) {
		if !p.loading {
			p.selectDraft(r)
		}
	})
	left.AddWidget(p.list.QWidget)

	btns := qt.NewQHBoxLayout2()
	add := qt.NewQToolButton2()
	add.SetText("+")
	add.SetToolTip("Add an agent")
	add.SetPopupMode(qt.QToolButton__InstantPopup)
	addMenu := qt.NewQMenu2()
	for _, vn := range agent.Vendors {
		vn := vn
		addMenu.AddActionWithText(vn.Name).OnTriggered(func() { p.addAgent(vn) })
	}
	add.SetMenu(addMenu)
	remove := qt.NewQToolButton2()
	remove.SetText("−")
	remove.SetToolTip("Remove the selected agent")
	remove.OnClicked(p.removeSelected)
	btns.AddWidget(add.QWidget)
	btns.AddWidget(remove.QWidget)
	btns.AddStretch()
	left.AddLayout(btns.QLayout)
	row.AddLayout(left.QLayout)

	// Right: the editor, or an empty-state hint.
	p.stack = qt.NewQStackedWidget2()
	p.empty = qt.NewQWidget2()
	ev := qt.NewQVBoxLayout(p.empty)
	emptyLabel := qt.NewQLabel3("No agent selected. Use + to add one.")
	setName(emptyLabel.QObject, "EmptyState")
	ev.AddStretch()
	ev.AddWidget(emptyLabel.QWidget)
	ev.AddStretch()
	p.editor = p.buildEditor()
	p.stack.AddWidget(p.empty)
	p.stack.AddWidget(p.editor)
	detailPanel := qt.NewQFrame2()
	setName(detailPanel.QObject, "Panel")
	detailPanel.SetFrameShape(qt.QFrame__StyledPanel)
	dp := qt.NewQVBoxLayout(detailPanel.QWidget)
	dp.SetContentsMargins(12, 10, 12, 10)
	dp.AddWidget(p.stack.QWidget)
	row.AddWidget2(detailPanel.QWidget, 1)

	v.AddLayout2(row.QLayout, 1)

	v.AddSpacing(8)
	v.AddWidget(p.buildMCPSection())

	return &settingsPage{
		title:       settingsAgents,
		description: "AI agents and the MCP server they use.",
		body:        body,
		load:        func(config.Config) { p.load() },
		store:       func(c *config.Config) { p.store(c) },
	}, p
}

func (p *agentsPage) buildEditor() *qt.QWidget {
	w := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(w)
	v.SetContentsMargins(0, 0, 0, 0)
	form := newSettingsForm()

	p.name = qt.NewQLineEdit2()
	p.vendor = qt.NewQComboBox2()
	for _, vn := range agent.Vendors {
		p.vendor.AddItem(vn.Name)
	}
	p.model = qt.NewQLineEdit2()
	p.baseURL = qt.NewQLineEdit2()
	p.key = qt.NewQLineEdit2()
	p.key.SetEchoMode(qt.QLineEdit__Password)
	p.key.SetPlaceholderText("Paste the API key")

	show := qt.NewQCheckBox3("Show")
	show.OnToggled(func(on bool) {
		if on {
			p.key.SetEchoMode(qt.QLineEdit__Normal)
		} else {
			p.key.SetEchoMode(qt.QLineEdit__Password)
		}
	})
	keyRow := qt.NewQWidget2()
	kr := qt.NewQHBoxLayout(keyRow)
	kr.SetContentsMargins(0, 0, 0, 0)
	kr.AddWidget2(p.key.QWidget, 1)
	kr.AddWidget(show.QWidget)
	p.keyRow = keyRow

	// Whether this agent reviews new findings. A background session scores
	// each one's confidence as it is recorded, so the noise can be filtered
	// out on the Findings page. On by default for a new agent.
	p.review = qt.NewQCheckBox3("Watch for and review new findings")
	p.review.SetToolTip("Have this agent score each new finding's confidence (0–100) in the background, " +
		"so likely false positives can be hidden from the Findings page.")

	form.AddRow3("Name", p.name.QWidget)
	form.AddRow3("Vendor", p.vendor.QWidget)
	form.AddRow3("Model", p.model.QWidget)
	form.AddRow3("API base URL", p.baseURL.QWidget)
	form.AddRow3("API key", keyRow)
	form.AddRow3("Findings", p.review.QWidget)
	v.AddLayout(form.QLayout)
	v.AddStretch()

	p.name.OnTextEdited(func(s string) { p.edit(func(d *agentDraft) { d.profile.Name = s }) })
	p.model.OnTextEdited(func(s string) { p.edit(func(d *agentDraft) { d.profile.Model = s }) })
	p.baseURL.OnTextEdited(func(s string) { p.edit(func(d *agentDraft) { d.profile.BaseURL = s }) })
	p.key.OnTextEdited(func(s string) { p.edit(func(d *agentDraft) { d.key = s }) })
	p.review.OnToggled(func(on bool) {
		p.edit(func(d *agentDraft) { v := on; d.profile.ReviewFindings = &v })
	})
	p.vendor.OnActivated(func(i int) {
		if i >= 0 && i < len(agent.Vendors) {
			p.changeVendor(agent.Vendors[i])
		}
	})
	return w
}

func (p *agentsPage) buildMCPSection() *qt.QWidget {
	w := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(w)
	v.SetContentsMargins(0, 0, 0, 0)
	v.SetSpacing(8)
	v.AddWidget(sectionTitle("MCP server"))
	v.AddWidget(helpText("Agents reach reproxy through an MCP server. It starts with the " +
		"reproxy opens and is shared across sessions, so an external agent such as Claude " +
		"Code can attach at any time; its status is in the footer. " +
		"External clients authenticate with the bearer token below.").QWidget)

	form := newSettingsForm()
	p.mcpPort = qt.NewQSpinBox2()
	p.mcpPort.SetRange(1, 65535)
	p.mcpPort.SetButtonSymbols(qt.QAbstractSpinBox__NoButtons)
	p.mcpPolicy = qt.NewQComboBox2()
	for _, o := range externalPolicyOptions {
		p.mcpPolicy.AddItem(o.label)
	}
	form.AddRow3("Port", p.mcpPort.QWidget)
	form.AddRow3("External clients", p.mcpPolicy.QWidget)
	v.AddLayout(form.QLayout)

	// Token row: masked field, Show, Copy, Regenerate.
	p.token = qt.NewQLineEdit2()
	p.token.SetReadOnly(true)
	p.token.SetEchoMode(qt.QLineEdit__Password)
	showTok := qt.NewQCheckBox3("Show")
	showTok.OnToggled(func(on bool) {
		if on {
			p.token.SetEchoMode(qt.QLineEdit__Normal)
		} else {
			p.token.SetEchoMode(qt.QLineEdit__Password)
		}
	})
	copyBtn := qt.NewQPushButton3("Copy")
	copyBtn.SetAutoDefault(false)
	copyBtn.OnClicked(func() { qt.QGuiApplication_Clipboard().SetText(p.token.Text()) })
	regen := qt.NewQPushButton3("Regenerate")
	regen.SetAutoDefault(false)
	regen.OnClicked(p.regenerateToken)
	tokRow := qt.NewQHBoxLayout2()
	tokRow.AddWidget(qt.NewQLabel3("Token").QWidget)
	tokRow.AddWidget2(p.token.QWidget, 1)
	tokRow.AddWidget(showTok.QWidget)
	tokRow.AddWidget(copyBtn.QWidget)
	tokRow.AddWidget(regen.QWidget)
	v.AddLayout(tokRow.QLayout)
	return w
}

// These live on the page but are built in buildMCPSection.
var externalPolicyOptions = []option[string]{
	{"Ask me each time", config.PolicyAsk},
	{"Allow automatically", config.PolicyAuto},
	{"Read-only (refuse changes)", config.PolicyReadOnly},
}

// ---- Data flow -------------------------------------------------------------

// load fills the page from the controller.
func (p *agentsPage) load() {
	ag := p.ctrl.AgentConfig()
	p.drafts = p.drafts[:0]
	for _, prof := range ag.Profiles {
		p.drafts = append(p.drafts, agentDraft{profile: prof, key: p.ctrl.APIKey(prof.ID)})
	}
	p.refreshList()
	if len(p.drafts) > 0 {
		p.list.SetCurrentRow(0)
	} else {
		p.selectDraft(-1)
	}

	mcp := p.ctrl.Settings().MCP
	p.mcpPort.SetValue(mcp.Port)
	selectOption(p.mcpPolicy, externalPolicyOptions, mcp.ExternalPolicy)
	if tok, err := p.ctrl.MCPToken(); err == nil {
		p.token.SetText(tok)
	}
}

// store copies the page's settings into c. Besides the MCP fields that is
// the user's acknowledgement of what an agent can read, which is never
// cleared here: once seen, it stays seen.
func (p *agentsPage) store(c *config.Config) {
	c.MCP.Port = p.mcpPort.Value()
	c.MCP.ExternalPolicy = selectedOption(p.mcpPolicy, externalPolicyOptions)
	if p.warned {
		c.AgentWarningSeen = true
	}
}

// drafts and keys returns the current agent configuration for saving.
func (p *agentsPage) agentsAndKeys() (config.Agents, map[string]string) {
	ag := config.Agents{}
	keys := make(map[string]string)
	for _, d := range p.drafts {
		ag.Profiles = append(ag.Profiles, d.profile)
		keys[d.profile.ID] = d.key
	}
	return ag, keys
}

func (p *agentsPage) refreshList() {
	p.loading = true
	for p.list.Count() > 0 {
		p.list.TakeItem(0)
	}
	for _, d := range p.drafts {
		p.list.AddItem(agentListLabel(d.profile))
	}
	p.loading = false
}

func agentListLabel(prof config.AgentProfile) string {
	if v, ok := agent.VendorByID(prof.Vendor); ok {
		return prof.Name + "  ·  " + v.Name
	}
	return prof.Name
}

func (p *agentsPage) selectDraft(row int) {
	p.selected = row
	if row < 0 || row >= len(p.drafts) {
		p.stack.SetCurrentWidget(p.empty)
		return
	}
	p.stack.SetCurrentWidget(p.editor)
	d := p.drafts[row]
	p.loading = true
	p.name.SetText(d.profile.Name)
	p.model.SetText(d.profile.Model)
	p.baseURL.SetText(d.profile.BaseURL)
	p.key.SetText(d.key)
	p.review.SetChecked(d.profile.ReviewsFindings())
	for i, v := range agent.Vendors {
		if v.ID == d.profile.Vendor {
			p.vendor.SetCurrentIndex(i)
			break
		}
	}
	p.applyVendorUI(d.profile.Vendor)
	p.loading = false
}

// applyVendorUI hides the key row for vendors that take no key.
func (p *agentsPage) applyVendorUI(vendorID string) {
	needsKey := true
	if v, ok := agent.VendorByID(vendorID); ok {
		needsKey = v.NeedsKey
		p.baseURL.SetPlaceholderText(v.DefaultBaseURL)
	}
	p.keyRow.SetVisible(needsKey)
}

func (p *agentsPage) edit(mutate func(*agentDraft)) {
	if p.loading || p.selected < 0 || p.selected >= len(p.drafts) {
		return
	}
	mutate(&p.drafts[p.selected])
	item := p.list.Item(p.selected)
	if item != nil {
		item.SetText(agentListLabel(p.drafts[p.selected].profile))
	}
}

func (p *agentsPage) addAgent(v agent.Vendor) {
	// What an agent can read of the traffic is worth knowing before there
	// is one, not after. Backing out of it adds nothing.
	if !p.warned && !p.ctrl.Settings().AgentWarningSeen {
		if !confirmAgentAccess(p.list.QWidget, v) {
			return
		}
		p.warned = true
	}
	p.drafts = append(p.drafts, agentDraft{profile: config.AgentProfile{
		ID:      newAgentID(),
		Vendor:  v.ID,
		Name:    uniqueAgentName(p.drafts, v.Name),
		Model:   v.DefaultModel,
		BaseURL: "",
	}})
	p.refreshList()
	p.list.SetCurrentRow(len(p.drafts) - 1)
}

// confirmAgentAccess warns, before the first agent is added, what an agent
// can read of the captured traffic and where that goes, and asks the user
// to go on. It is asked once (config.Config.AgentWarningSeen) and is modal
// to the Settings window only.
//
// It is a warning and not a setting because there is nothing to configure:
// an agent that could not read the traffic would be no use, so the only
// thing the user can do with this is decide what they put through the
// proxy while one is configured.
func confirmAgentAccess(parent *qt.QWidget, v agent.Vendor) bool {
	host := endpointHost(v.DefaultBaseURL)
	ok, _ := confirm(parent, confirmOptions{
		title:   "Add an AI agent?",
		heading: "An agent can read all of your traffic",
		body: []string{
			"An agent you add can read everything reproxy captures: every request and response " +
				"that goes through the proxy, headers and bodies alike, along with your findings " +
				"and the Entity Explorer. It reads whatever happens to be in them, not only the " +
				"part you are testing — session cookies, passwords, API keys and tokens, personal " +
				"data, anything a page or an API returns.",
			fmt.Sprintf("What it reads is sent to the model to be answered: with %s, to %s. %s",
				v.Name, host, destination(host)),
			"So be deliberate about what goes through the proxy while an agent is configured. " +
				"Keep to what you are authorised to test, and sign out of anything unrelated — " +
				"personal mail, banking, work accounts — before you point a browser at it.",
		},
		cancel: "Nevermind, Cancel",
		ok:     "I understand, Add the Agent",
	})
	return ok
}

// endpointHost is the host an agent's traffic goes to, for the warning:
// "localhost:11434" for a model on this machine, "api.anthropic.com" for a
// hosted one. An unparseable URL is quoted as it stands rather than hidden.
func endpointHost(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return baseURL
	}
	return u.Host
}

// destination finishes the sentence that names where an agent's traffic
// goes. A model on this machine is not a third party, and saying it is
// would be the kind of inaccuracy that teaches people to skip warnings;
// what is worth saying about it is that the API base URL decides, and it
// can be pointed anywhere.
func destination(host string) string {
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	switch strings.ToLower(name) {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return "That stays on this machine — but the API base URL decides, " +
			"and pointing it elsewhere sends the traffic there."
	}
	return "Treat that the way you would any other party you hand a capture to; " +
		"the API base URL decides, and it can be pointed elsewhere."
}

// removeSelected drops the selected agent after asking. It asks because
// agentsAndKeys rebuilds the key map from the drafts that are left, so
// saving after a removal deletes that profile's API key from the secrets
// file — the user would have to fetch it from the vendor again.
func (p *agentsPage) removeSelected() {
	if p.selected < 0 || p.selected >= len(p.drafts) {
		return
	}
	d := p.drafts[p.selected]
	body := []string{
		"Its model, endpoint and settings are removed from this page. " +
			"Nothing it has already done is undone: chat sessions it ran are kept, " +
			"as are any findings it recorded.",
	}
	if d.key != "" {
		body = append(body, "Its API key is removed from the secrets file when you apply "+
			"these settings. reProxy cannot get it back — you would have to fetch it "+
			"from the vendor again.")
	}
	if ok, _ := confirm(p.list.QWidget, confirmOptions{
		title:   "Remove agent",
		heading: "Remove \"" + agentListLabel(d.profile) + "\"?",
		body:    body,
		cancel:  "Cancel",
		ok:      "Remove the Agent",
	}); !ok {
		return
	}
	p.drafts = append(p.drafts[:p.selected], p.drafts[p.selected+1:]...)
	p.refreshList()
	switch {
	case len(p.drafts) == 0:
		p.list.SetCurrentRow(-1)
		p.selectDraft(-1)
	default:
		p.list.SetCurrentRow(min(p.selected, len(p.drafts)-1))
	}
}

// changeVendor switches the selected agent's vendor, filling in that
// vendor's default model if the model field was still a default.
func (p *agentsPage) changeVendor(v agent.Vendor) {
	p.edit(func(d *agentDraft) {
		old, _ := agent.VendorByID(d.profile.Vendor)
		if d.profile.Model == "" || d.profile.Model == old.DefaultModel {
			d.profile.Model = v.DefaultModel
			p.loading = true
			p.model.SetText(v.DefaultModel)
			p.loading = false
		}
		d.profile.Vendor = v.ID
	})
	p.applyVendorUI(v.ID)
}

func (p *agentsPage) regenerateToken() {
	if qt.QMessageBox_Question2(p.token.QWidget, "Regenerate token",
		"Any MCP client using the current token will stop working until you give it the new one. Continue?",
		qt.QMessageBox__Yes, qt.QMessageBox__No) != int(qt.QMessageBox__Yes) {
		return
	}
	tok, err := p.ctrl.RegenerateMCPToken()
	if err != nil {
		qt.QMessageBox_Warning(p.token.QWidget, "Couldn't regenerate token", err.Error())
		return
	}
	p.token.SetText(tok)
}

// newAgentID returns a short random profile ID.
func newAgentID() string { return randid.Hex(6) }

// uniqueAgentName appends a number to base if a draft already uses it.
func uniqueAgentName(drafts []agentDraft, base string) string {
	used := func(name string) bool {
		for _, d := range drafts {
			if d.profile.Name == name {
				return true
			}
		}
		return false
	}
	if !used(base) {
		return base
	}
	for i := 2; ; i++ {
		name := fmt.Sprintf("%s %d", base, i)
		if !used(name) {
			return name
		}
	}
}
