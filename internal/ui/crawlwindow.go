package ui

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"reproxy/internal/crawl"
	"reproxy/internal/target"
)

// crawlWindow is the Autocrawl window, opened from the crawler button at
// the right of the menu bar. It is a top-level window of its own rather
// than a dialog, so the user can go on working in Traffic, Findings and the
// Explorer while the crawl fills them, and closing it leaves the crawl
// running. It only presents and calls: the crawl itself is crawl.Crawler.
type crawlWindow struct {
	w       *qt.QWidget
	crawler *crawl.Crawler
	onFlow  func(uint64)

	status   *qt.QLabel
	detail   *qt.QLabel
	problem  *qt.QLabel
	start    *qt.QPushButton
	stop     *qt.QPushButton
	rate     *qt.QDoubleSpinBox
	threads  *qt.QSpinBox
	onlyNew  *qt.QCheckBox
	apiDocs  *qt.QCheckBox
	counts   map[string]*qt.QLabel
	activity *qt.QTreeWidget
	timer    *qt.QTimer

	// shown is the newest result in the activity list, so the list is only
	// rebuilt when there is something new.
	shown      crawl.Result
	shownCount int
	// loading is set while the controls are filled from the crawler's
	// settings, so doing so doesn't save them back.
	loading bool

	// The target selector is rebuilt whenever the targets change: targets
	// is the store, targetRow holds the selector, and targetsShown says
	// which targets it was built for, so an unrelated save leaves it alone.
	targets      *target.Store
	targetRow    *qt.QHBoxLayout
	targetPick   *checkFilter
	targetsShown string
	unsubTargets func()
}

// crawlRefresh is how often the window reads the crawl's status.
const crawlRefresh = 500 * time.Millisecond

func newCrawlWindow(parent *qt.QWidget, ctrl Controller, onFlow func(uint64)) *crawlWindow {
	// A window of its own, but the main window's child: it is closed and
	// destroyed with it, and on its own it never keeps reproxy running
	// (WA_QuitOnClose), so closing the main window ends the process even
	// while a crawl is on screen.
	c := &crawlWindow{w: qt.NewQWidget(parent), crawler: ctrl.Crawler(), onFlow: onFlow, counts: map[string]*qt.QLabel{},
		targets: ctrl.Targets()}
	c.w.SetWindowTitle("Autocrawl")
	c.w.SetWindowFlags(qt.Window)
	c.w.SetAttribute2(qt.WA_QuitOnClose, false)
	// As large as it likes to be, but never larger than the screen it opens
	// on (a laptop at 150% has little more than 900×500 to give).
	width, height, minW, minH := 820, 700, 560, 420
	if scr := qt.QGuiApplication_PrimaryScreen(); scr != nil {
		avail := scr.AvailableGeometry()
		width, height = min(width, avail.Width()-40), min(height, avail.Height()-60)
		minW, minH = min(minW, width), min(minH, height)
	}
	c.w.Resize(width, height)
	c.w.SetMinimumSize2(minW, minH)

	v := qt.NewQVBoxLayout(c.w)
	v.SetContentsMargins(18, 16, 18, 16)
	v.SetSpacing(12)

	// Header: what is happening, and the buttons that change it.
	head := qt.NewQHBoxLayout2()
	head.SetSpacing(10)
	titles := qt.NewQVBoxLayout2()
	titles.SetSpacing(2)
	title := qt.NewQLabel3("Autocrawl")
	setName(title.QObject, "SettingsPageTitle")
	titles.AddWidget(title.QWidget)
	c.status = qt.NewQLabel2()
	titles.AddWidget(c.status.QWidget)
	head.AddLayout2(titles.QLayout, 1)
	c.stop = qt.NewQPushButton3("Stop")
	c.stop.SetToolTip("Stop crawling. What is left stays queued for the next start.")
	c.stop.OnClicked(c.stopCrawl)
	c.start = qt.NewQPushButton3("Start")
	setName(c.start.QObject, "PrimaryButton")
	c.start.SetToolTip("Start crawling the targets")
	c.start.OnClicked(c.startCrawl)
	head.AddWidget(c.stop.QWidget)
	head.AddWidget(c.start.QWidget)
	v.AddLayout(head.QLayout)

	// Everything between the header and the messages scrolls, so on a
	// small or scaled screen the window still fits and nothing is
	// squeezed: Start and Stop, and what went wrong, stay in view.
	body := qt.NewQWidget2()
	b := qt.NewQVBoxLayout(body)
	b.SetContentsMargins(0, 0, 4, 0) // room beside the scroll bar
	b.SetSpacing(12)

	b.AddWidget(helpText("Follows the links of your targets' pages, recursively, so the Explorer maps every URL, host " +
		"and domain they lead to. Only what a target covers is crawled; a link anywhere else waits, and is crawled " +
		"once you promote its host to a target. Every request goes out through the upstream proxy chain in force " +
		"(Settings → Outbound Traffic) and shows in Traffic marked CRAWL.").QWidget)

	// Settings. They apply to a whole run, so they are locked while one is
	// going.
	b.AddWidget(sectionTitle("What to crawl"))
	what := newSettingsForm()
	pick := qt.NewQWidget2()
	c.targetRow = qt.NewQHBoxLayout(pick)
	c.targetRow.SetContentsMargins(0, 0, 0, 0)
	c.targetRow.AddStretch()
	what.AddRow3("Targets", pick)
	b.AddLayout(what.QLayout)
	c.onlyNew = qt.NewQCheckBox3("Only crawl new URLs")
	c.onlyNew.SetToolTip("Skip what earlier runs crawled. Off, starting crawls everything again (each URL once).")
	c.onlyNew.OnToggled(func(bool) { c.saveSettings() })
	b.AddWidget(c.onlyNew.QWidget)
	c.apiDocs = qt.NewQCheckBox3("Look for API docs")
	c.apiDocs.SetToolTip("Try the usual places OpenAPI/Swagger descriptions are kept on each host, until one is found there")
	c.apiDocs.OnToggled(func(bool) { c.saveSettings() })
	b.AddWidget(c.apiDocs.QWidget)
	b.AddWidget(helpText("The crawl follows what it is led to: the URLs already known under the chosen targets, " +
		"each host's robots.txt and sitemap, and every link, script reference and API description they turn up, " +
		"recursively. “Look for API docs” also guesses at the usual OpenAPI/Swagger locations " +
		fmt.Sprintf("(%d requests a host) until a description is found there. Leave “Only crawl new URLs” on to ", len(crawl.APIDocPaths)) +
		"extend a crawl to hosts you have just promoted without fetching again what was crawled before.").QWidget)

	b.AddWidget(sectionTitle("Speed"))
	form := newSettingsForm()
	c.rate = qt.NewQDoubleSpinBox2()
	c.rate.SetRange(crawl.MinRequestsPerSecond, crawl.MaxRequestsPerSecond)
	c.rate.SetDecimals(1)
	c.rate.SetSingleStep(0.5)
	c.rate.SetButtonSymbols(qt.QAbstractSpinBox__NoButtons) // typed, as the app's numbers are
	c.rate.SetMinimumWidth(120)
	c.rate.SetToolTip("The most requests started in any second, across all threads")
	c.rate.OnValueChanged(func(float64) { c.saveSettings() })
	c.threads = qt.NewQSpinBox2()
	c.threads.SetRange(1, crawl.MaxThreads())
	c.threads.SetButtonSymbols(qt.QAbstractSpinBox__NoButtons)
	c.threads.SetMinimumWidth(120)
	c.threads.SetToolTip(fmt.Sprintf("The most requests in flight at once (up to %d on this machine)", crawl.MaxThreads()))
	c.threads.OnValueChanged(func(int) { c.saveSettings() })
	form.AddRow3("Requests per second", c.rate.QWidget)
	form.AddRow3("Threads", c.threads.QWidget)
	b.AddLayout(form.QLayout)
	b.AddWidget(helpText(fmt.Sprintf("Both limits are strict: requests start at least 1/rate seconds apart, and no more "+
		"than the threads are in flight at once (at most %d here, one per CPU).", crawl.MaxThreads())).QWidget)

	// Where the crawl stands.
	b.AddWidget(sectionTitle("Progress"))
	row := qt.NewQHBoxLayout2()
	row.SetSpacing(22)
	for _, k := range []struct{ key, label, tip string }{
		{"queued", "Queued", "In scope and not yet fetched"},
		{"crawled", "Crawled", "Fetched, whatever the status code"},
		{"failed", "Failed", "Couldn't be fetched (connection, TLS, timeout)"},
		{"waiting", "Waiting on a target", "Linked to on a host no target covers, or under a target this crawl leaves out: promote or choose it to crawl them"},
		{"inflight", "In flight", "Requests started or waiting for their slot"},
		{"sent", "Sent this run", "Requests this run has made"},
	} {
		box := qt.NewQVBoxLayout2()
		box.SetSpacing(0)
		n := qt.NewQLabel3("0")
		setName(n.QObject, "PanelTitle")
		n.SetToolTip(k.tip)
		l := qt.NewQLabel3(k.label)
		setName(l.QObject, "Muted")
		l.SetToolTip(k.tip)
		box.AddWidget(n.QWidget)
		box.AddWidget(l.QWidget)
		row.AddLayout(box.QLayout)
		c.counts[k.key] = n
	}
	row.AddStretch()
	b.AddLayout(row.QLayout)

	c.activity = qt.NewQTreeWidget2()
	setName(c.activity.QObject, "RequestsTable")
	c.activity.SetRootIsDecorated(false)
	c.activity.SetUniformRowHeights(true)
	c.activity.SetAllColumnsShowFocus(true)
	c.activity.SetEditTriggers(qt.QAbstractItemView__NoEditTriggers)
	cols := []string{"Result", "URL", "New links", "Found on", "At"}
	c.activity.SetColumnCount(len(cols))
	c.activity.SetHeaderLabels(cols)
	header := c.activity.Header()
	header.SetStretchLastSection(false)
	header.SetSectionResizeMode2(0, qt.QHeaderView__ResizeToContents)
	header.SetSectionResizeMode2(1, qt.QHeaderView__Stretch)
	header.SetSectionResizeMode2(2, qt.QHeaderView__ResizeToContents)
	header.SetSectionResizeMode2(3, qt.QHeaderView__Interactive)
	header.SetSectionResizeMode2(4, qt.QHeaderView__ResizeToContents)
	header.ResizeSection(3, 220)
	c.activity.SetToolTip("The latest URLs crawled. Double-click one to see its request in Traffic.")
	c.activity.OnItemDoubleClicked(func(item *qt.QTreeWidgetItem, _ int) {
		if id, err := strconv.ParseUint(item.Data(0, int(qt.UserRole)).ToString(), 10, 64); err == nil && id != 0 {
			c.onFlow(id)
		}
	})
	c.activity.SetMinimumHeight(140)
	b.AddWidget2(c.activity.QWidget, 1)
	scroll := qt.NewQScrollArea2()
	scroll.SetWidgetResizable(true)
	scroll.SetFrameShape(qt.QFrame__NoFrame)
	scroll.SetHorizontalScrollBarPolicy(qt.ScrollBarAlwaysOff)
	scroll.SetWidget(body)
	v.AddWidget2(scroll.QWidget, 1)

	c.detail = helpText("")
	v.AddWidget(c.detail.QWidget)
	c.problem = qt.NewQLabel2()
	setName(c.problem.QObject, "ErrorText")
	c.problem.SetWordWrap(true)
	c.problem.SetVisible(false)
	v.AddWidget(c.problem.QWidget)

	c.timer = qt.NewQTimer2(c.w.QObject)
	c.timer.SetInterval(int(crawlRefresh / time.Millisecond))
	c.timer.OnTimeout(c.refresh)
	return c
}

// show opens (or raises) the window with the crawl's current state.
func (c *crawlWindow) show() {
	c.loadSettings()
	c.refresh()
	c.timer.Start2()
	c.w.Show()
	c.w.Raise()
	c.w.ActivateWindow()
}

// close shuts the window for good, as the main window closes.
func (c *crawlWindow) close() {
	c.timer.Stop()
	if c.unsubTargets != nil {
		c.unsubTargets()
	}
	c.w.Close()
}

func (c *crawlWindow) loadSettings() {
	s := c.crawler.Settings()
	c.loading = true
	c.rate.SetValue(s.RequestsPerSecond)
	c.threads.SetValue(s.Threads)
	c.onlyNew.SetChecked(s.OnlyNew)
	c.apiDocs.SetChecked(s.APIDocs)
	c.targetsShown = "" // rebuilt with the saved choice
	c.rebuildTargets(s.ExcludedTargets)
	if c.unsubTargets == nil {
		c.unsubTargets = c.targets.Subscribe(func() {
			mainthread.Start(func() { c.rebuildTargets(c.excluded()) })
		})
	}
	c.loading = false
}

// rebuildTargets puts a selector for the current targets in the window,
// with those in excluded unticked. A target is ticked unless the user
// unticked it, so one added later is crawled.
func (c *crawlWindow) rebuildTargets(excluded []string) {
	list := c.targets.List()
	var key strings.Builder
	entries := make([]filterEntry, 0, len(list))
	for _, t := range list {
		entries = append(entries, filterEntry{id: t.ID, label: t.Label(), color: t.Color})
		fmt.Fprintf(&key, "%s\x00%s\x00%s\x00", t.ID, t.Label(), t.Color)
	}
	if key.String() == c.targetsShown && c.targetPick != nil {
		return
	}
	c.targetsShown = key.String()
	if c.targetPick != nil {
		old := c.targetPick.widget()
		c.targetRow.RemoveWidget(old)
		old.DeleteLater()
	}
	n := len(entries)
	c.targetPick = newCheckFilter("Which targets this crawl covers", entries,
		func(string, bool) { c.saveSettings() },
		func(hidden int) string {
			switch {
			case n == 0:
				return "No targets yet"
			case hidden == 0:
				return fmt.Sprintf("All targets (%d)", n)
			case hidden == n:
				return "No targets"
			}
			return fmt.Sprintf("%d of %d targets", n-hidden, n)
		})
	c.targetPick.setHidden(excluded, false)
	c.targetPick.widget().SetEnabled(n > 0 && !c.crawler.Status().Running)
	c.targetRow.InsertWidget(0, c.targetPick.widget())
}

// excluded is the targets unticked in the selector.
func (c *crawlWindow) excluded() []string {
	if c.targetPick == nil {
		return nil
	}
	return c.targetPick.hiddenIDs()
}

func (c *crawlWindow) settings() crawl.Settings {
	return crawl.Settings{
		RequestsPerSecond: c.rate.Value(), Threads: c.threads.Value(),
		OnlyNew: c.onlyNew.IsChecked(), APIDocs: c.apiDocs.IsChecked(),
		ExcludedTargets: c.excluded(),
	}
}

// saveSettings keeps an edit to the settings with the engagement, so they
// are there next time even if no crawl is started.
func (c *crawlWindow) saveSettings() {
	if c.loading {
		return
	}
	if err := c.crawler.SaveSettings(c.settings()); err != nil && !errors.Is(err, crawl.ErrRunning) {
		c.showProblem(err.Error())
	}
}

func (c *crawlWindow) startCrawl() {
	c.showProblem("")
	settings := c.settings()
	if n := len(c.targets.List()); n > 0 && len(settings.ExcludedTargets) >= n {
		c.showProblem("Choose at least one target to crawl.")
		return
	}
	if !c.crawler.WarningDismissed() {
		ok, dismiss := confirmCrawl(c.w, settings)
		if !ok {
			return
		}
		if dismiss {
			if err := c.crawler.DismissWarning(); err != nil {
				c.showProblem(err.Error())
			}
		}
	}
	if err := c.crawler.Start(settings); err != nil {
		c.showProblem(err.Error())
	}
	c.refresh()
}

// confirmCrawl warns, before a crawl starts, what a crawl does to the
// servers it reaches, and asks the user to go on. It is modal to the
// Autocrawl window only, so the rest of the app stays usable. dismiss is
// set when the user asks not to be warned again in this engagement.
func confirmCrawl(parent *qt.QWidget, s crawl.Settings) (ok, dismiss bool) {
	return confirm(parent, confirmOptions{
		title:   "Start Autocrawl?",
		heading: "Autocrawl is noisy",
		body: []string{
			"Autocrawl requests every URL it can find on your targets: their pages, scripts and API endpoints, " +
				"their robots.txt and sitemaps and, if you asked, the places API descriptions are usually kept. " +
				"That is a lot of traffic. It shows up in the target's logs and monitoring, and if it is abused " +
				"(run too fast, too wide, or against a server that can't take it) it can put real stress on the " +
				"web server, or set off rate limits and alarms.",
			fmt.Sprintf("Only crawl what you are authorised to test, and keep the rate low against anything "+
				"shared or in production. This crawl will start up to %s a second, with %s in flight at once.",
				rateText(s.RequestsPerSecond), countOf(s.Threads, "request", "requests")),
		},
		check:  "Don't show this again in this engagement",
		cancel: "Nevermind, Cancel",
		ok:     "I understand, Continue",
	})
}

func (c *crawlWindow) stopCrawl() {
	c.crawler.Stop()
	c.refresh()
}

func (c *crawlWindow) showProblem(text string) {
	c.problem.SetText(sentence(text))
	c.problem.SetVisible(text != "")
}

// refresh redraws the window from the crawl's status. It runs on a timer
// while the window is open, and stops while it is hidden.
func (c *crawlWindow) refresh() {
	if !c.w.IsVisible() && c.timer.IsActive() {
		c.timer.Stop()
		return
	}
	st := c.crawler.Status()

	text, tone := "Idle", colorMuted
	switch {
	case st.Stopping:
		text, tone = fmt.Sprintf("Stopping — waiting for %s", countOf(st.InFlight, "request", "requests")), colorHeld
	case st.Running:
		text, tone = fmt.Sprintf("Running · %s/s · %s", trimFloat(st.Settings.RequestsPerSecond),
			countOf(st.Settings.Threads, "thread", "threads")), colorSuccess
	case st.Ended == "finished":
		text, tone = "Finished — nothing left in scope to crawl", colorAccent
	case st.Ended == "stopped":
		text = "Stopped — press Start to carry on"
	}
	c.status.SetText(text)
	c.status.SetStyleSheet(fmt.Sprintf("color: %s; font-weight: 600;", hex(tone)))

	c.start.SetEnabled(!st.Running)
	c.stop.SetEnabled(st.Running && !st.Stopping)
	for _, w := range []*qt.QWidget{c.rate.QWidget, c.threads.QWidget, c.onlyNew.QWidget, c.apiDocs.QWidget} {
		w.SetEnabled(!st.Running)
	}
	if c.targetPick != nil {
		c.targetPick.widget().SetEnabled(!st.Running && len(c.targetPick.entries) > 0)
	}

	for k, n := range map[string]int{
		"queued": st.Queued, "crawled": st.Crawled, "failed": st.Failed,
		"waiting": st.Waiting, "inflight": st.InFlight, "sent": st.Sent,
	} {
		c.counts[k].SetText(strconv.Itoa(n))
	}

	var detail string
	switch {
	case st.Running:
		detail = "Started " + st.Started.Format("15:04:05") + "."
	case !st.Finished.IsZero():
		detail = fmt.Sprintf("Last run %s–%s, %s.", st.Started.Format("15:04:05"), st.Finished.Format("15:04:05"),
			countOf(st.Sent, "request", "requests"))
	}
	if !st.Recording {
		detail += " No engagement is open, so this crawl is forgotten when reProxy closes."
	}
	c.detail.SetText(detail)
	if st.WriteError != "" {
		c.showProblem(st.WriteError)
	}

	c.refreshActivity()
}

func (c *crawlWindow) refreshActivity() {
	recent := c.crawler.Recent()
	if len(recent) == c.shownCount && (len(recent) == 0 || recent[0] == c.shown) {
		return
	}
	c.shownCount = len(recent)
	if len(recent) > 0 {
		c.shown = recent[0]
	}
	c.activity.Clear()
	for _, r := range recent {
		item := qt.NewQTreeWidgetItem3(c.activity)
		result, tone := strconv.Itoa(r.Status), statusTone(r.Status)
		if r.State == crawl.Failed {
			result, tone = "failed", colorServerError
		}
		item.SetText(0, result)
		item.SetForeground(0, qt.NewQBrush3(color(tone)))
		item.SetText(1, r.URL)
		item.SetToolTip(1, r.URL)
		if r.Error != "" {
			item.SetToolTip(0, r.Error)
		}
		links := strconv.Itoa(r.Links)
		if r.Links < 0 {
			links = "—" // crawled in an earlier session
		}
		item.SetText(2, links)
		from := r.FoundOn
		if from == "" {
			from = "(where the crawl starts)"
		}
		item.SetText(3, from)
		item.SetToolTip(3, from)
		item.SetText(4, r.At.Format("15:04:05"))
		item.SetData(0, int(qt.UserRole), qt.NewQVariant14(strconv.FormatUint(r.FlowID, 10)))
	}
}

// statusTone colours a status code as the request list does.
func statusTone(code int) string {
	switch {
	case code >= 500:
		return colorServerError
	case code >= 400:
		return colorClientError
	case code >= 300:
		return colorRedirect
	case code >= 200:
		return colorSuccess
	}
	return colorMuted
}

// rateText is a number of requests that may be fractional: "1 request",
// "0.5 requests", "2.5 requests".
func rateText(f float64) string {
	if f == 1 {
		return "1 request"
	}
	return trimFloat(f) + " requests"
}

// trimFloat writes a rate without trailing zeros: 1, 0.5, 2.5.
func trimFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
