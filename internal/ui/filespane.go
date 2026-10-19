package ui

import (
	"fmt"
	"path/filepath"
	"strings"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"reproxy/internal/elide"
	"reproxy/internal/files"
)

// filesPane is the Files section of the main window: the files the work
// recovered, and what they hold.
//
// A row is one recovered file — its path under the engagement's recovered/
// folder, how big it is, what it is and who recovered it. Clicking a row
// opens the detail pane, which slides out of the right the way the Findings
// detail does: where the file came from, and the file itself in a read-only
// editor so it reads like any other message body in the app.
//
// Above the list are the filters: a text box matched against each file's
// path, notes, kind and source URL, a recovery picker (the files one
// unpacking produced are one batch), and a toggle that leaves out what
// cannot be read as text — the fonts and images a build ships beside its
// source.
//
// The files shown belong to the open engagement; opening another one
// replaces them, which arrives here as a store event like any other. Those
// events come from whichever goroutine recovered the file — a plugin's, an
// agent's — so they are handed to the main thread before anything is
// touched. Reading a file's bytes is done here, on demand, and never for a
// file that is merely listed: a recovery can be thousands of files.
type filesPane struct {
	root  *qt.QWidget
	store *files.Store

	list     *qt.QListWidget
	count    *qt.QLabel
	pages    *qt.QStackedWidget // 0: empty state, 1: the list
	search   *qt.QLineEdit
	batches  *qt.QComboBox
	batchIDs []string // the recovery each row of batches names
	textOnly *qt.QCheckBox

	split       *qt.QSplitter
	detail      *fileDetail
	detailPanel *qt.QWidget
	detailOpen  bool
	detailWidth int
	anim        *qt.QVariantAnimation

	// shown maps a list row to the file it draws, and current is the
	// selected file, kept across rebuilds so a recovery finishing elsewhere
	// doesn't move the selection.
	shown   []uint64
	current uint64
	// batch is the recovery the list is limited to, or "" for all of them.
	batch string
	// rebuilding guards the selection handler while rows are replaced.
	rebuilding bool

	unsubscribe func()
	// onShowFlow is asked to bring the request a file came from into view
	// in the Traffic section.
	onShowFlow func(flowID uint64)
}

const (
	fileRowHeight = 52
	fileListShare = 46 // percent of the section's width the detail pane takes
)

func newFilesPane(store *files.Store, onShowFlow func(uint64)) *filesPane {
	p := &filesPane{root: qt.NewQWidget2(), store: store, onShowFlow: onShowFlow}

	layout := qt.NewQVBoxLayout(p.root)
	layout.SetContentsMargins(0, 0, 0, 0)
	layout.SetSpacing(0)

	p.detail = newFileDetail(fileDetailActions{
		onClose:    func() { p.setDetailOpen(false) },
		onRemove:   p.removeFile,
		onShowFlow: func(flowID uint64) { p.showFlow(flowID) },
		read:       p.read,
		root:       store.Root,
	})
	p.detailPanel = p.detail.root

	p.split = qt.NewQSplitter3(qt.Horizontal)
	p.split.SetChildrenCollapsible(false)
	p.split.SetHandleWidth(windowMargin)
	p.split.AddWidget(p.buildList())
	p.split.AddWidget(p.detailPanel)
	p.split.SetStretchFactor(0, 100-fileListShare)
	p.split.SetStretchFactor(1, fileListShare)
	p.detailPanel.Hide() // closed until a file is clicked

	p.anim = qt.NewQVariantAnimation2(p.root.QObject)
	p.anim.SetDuration(200)
	p.anim.SetEasingCurve(qt.NewQEasingCurve3(qt.QEasingCurve__OutCubic))
	p.anim.OnValueChanged(func(v *qt.QVariant) { p.sizeDetail(v.ToInt()) })
	p.anim.OnFinished(p.detailSettled)

	layout.AddWidget(p.split.QWidget)

	// A file can be recovered by a plugin or an agent, on its own goroutine.
	p.unsubscribe = store.Subscribe(func(files.Event) {
		mainthread.Start(p.reload)
	})
	p.reload()
	return p
}

func (p *filesPane) close() {
	if p.unsubscribe != nil {
		p.unsubscribe()
	}
}

func (p *filesPane) buildList() *qt.QWidget {
	p.list = qt.NewQListWidget2()
	setName(p.list.QObject, "FindingList")
	p.list.SetFrameShape(qt.QFrame__NoFrame)
	p.list.SetSelectionMode(qt.QAbstractItemView__SingleSelection)
	p.list.SetHorizontalScrollBarPolicy(qt.ScrollBarAlwaysOff)
	p.list.OnCurrentRowChanged(func(row int) {
		if !p.rebuilding {
			p.selectRow(row)
		}
	})
	p.list.OnClicked(func(*qt.QModelIndex) { p.openCurrent() })
	p.list.OnActivated(func(*qt.QModelIndex) { p.openCurrent() })
	p.list.OnKeyPressEvent(func(super func(*qt.QKeyEvent), e *qt.QKeyEvent) {
		if e.Key() == int(qt.Key_Escape) && p.detailOpen {
			p.setDetailOpen(false)
			return
		}
		super(e)
	})

	panel := qt.NewQFrame2()
	setName(panel.QObject, "Panel")
	v := qt.NewQVBoxLayout(panel.QWidget)
	v.SetContentsMargins(1, 1, 1, 1)
	v.SetSpacing(0)
	v.AddWidget(p.buildHeader())

	// An empty list explains what recovered files are rather than showing
	// nothing: nothing in the app recovers anything on its own.
	p.pages = qt.NewQStackedWidget2()
	p.pages.AddWidget(newEmptyState("Nothing recovered yet",
		"A recovered file is something pulled out of the traffic and written under this engagement: "+
			"the original source a JavaScript source map carries, say. Right-click a .map response in "+
			"Traffic and choose \"Export recovered sources…\", or let a plugin or an agent recover one. "+
			"Every file recovered is offered to the plugins that scan them."))
	p.pages.AddWidget(p.list.QWidget)
	v.AddWidget2(p.pages.QWidget, 1)
	return panel.QWidget
}

func (p *filesPane) buildHeader() *qt.QWidget {
	header := qt.NewQWidget2()
	setName(header.QObject, "PanelHeader")
	header.SetAttribute(qt.WA_StyledBackground)
	h := qt.NewQHBoxLayout(header)
	h.SetContentsMargins(rowPadX, 6, rowPadX-4, 6)
	h.SetSpacing(8)

	title := qt.NewQLabel3("Files")
	setName(title.QObject, "PanelTitle")
	p.count = qt.NewQLabel3("0")
	setName(p.count.QObject, "Badge")

	// One entry per recovery, so a source map's thousands of files can be
	// looked at on their own.
	p.batches = qt.NewQComboBox2()
	p.batches.SetToolTip("Show one recovery's files")
	p.batches.SetMinimumWidth(200)
	p.batches.OnActivated(func(i int) {
		if i >= 0 && i < len(p.batchIDs) {
			p.batch = p.batchIDs[i]
		}
		p.reload()
	})

	p.textOnly = qt.NewQCheckBox3("Text only")
	p.textOnly.SetToolTip("Leave out the files that aren't text: fonts, images, archives")
	p.textOnly.OnStateChanged(func(int) { p.reload() })

	p.search = qt.NewQLineEdit2()
	p.search.SetPlaceholderText("Filter files…")
	p.search.SetClearButtonEnabled(true)
	p.search.SetFixedWidth(240)
	p.search.OnTextChanged(func(string) { p.reload() })

	h.AddWidget(title.QWidget)
	h.AddWidget(p.count.QWidget)
	h.AddStretch()
	h.AddWidget(p.textOnly.QWidget)
	h.AddWidget(p.batches.QWidget)
	h.AddWidget(p.search.QWidget)
	h.AddWidget(newIconButton(iconTrash, "Delete every recovered file", p.clearFiles).w)
	return header
}

// reload rebuilds the rows from the store, keeping the selection, and
// refreshes the detail pane in case the shown file changed.
func (p *filesPane) reload() {
	p.rebuildBatches()

	q := files.Query{Batch: p.batch, Text: p.search.Text()}
	if p.textOnly != nil {
		q.TextOnly = p.textOnly.IsChecked()
	}
	list := p.store.Select(q)

	p.rebuilding = true
	p.list.Clear()
	p.shown = p.shown[:0]
	for _, f := range list {
		item := qt.NewQListWidgetItem()
		item.SetSizeHint(qt.NewQSize2(0, fileRowHeight))
		p.list.AddItemWithItem(item)
		p.list.SetItemWidget(item, p.buildRow(f))
		p.shown = append(p.shown, f.ID)
	}
	p.rebuilding = false

	p.count.SetText(countLabel(len(list), p.store.Len()))
	page := 1
	if len(list) == 0 {
		page = 0
	}
	p.pages.SetCurrentIndex(page)

	// Keep the selection on the same file where it still shows.
	row := -1
	for i, id := range p.shown {
		if id == p.current {
			row = i
		}
	}
	if row >= 0 {
		p.list.SetCurrentRow(row)
	}
	if f, ok := p.store.Get(p.current); ok {
		p.detail.show(f)
	} else if p.detailOpen {
		p.setDetailOpen(false) // the shown file is gone
	}
}

// rebuildBatches refills the recovery picker, keeping the chosen one where
// it still exists. batchIDs is the ids by row, since a combo box here holds
// text and nothing else.
func (p *filesPane) rebuildBatches() {
	list := p.store.Batches()
	p.batches.SetVisible(len(list) > 0)
	if len(list) == 0 {
		p.batch, p.batchIDs = "", nil
		return
	}
	found := p.batch == ""
	p.batches.Clear()
	p.batchIDs = p.batchIDs[:0]
	p.batches.AddItem("Every recovery")
	p.batchIDs = append(p.batchIDs, "")
	for _, b := range list {
		p.batches.AddItem(batchLabel(b))
		p.batchIDs = append(p.batchIDs, b.ID)
		if b.ID == p.batch {
			found = true
		}
	}
	if !found {
		p.batch = "" // the recovery it was limited to is gone
	}
	for i, id := range p.batchIDs {
		if id == p.batch {
			p.batches.SetCurrentIndex(i)
			break
		}
	}
}

// batchLabel names a recovery in the picker: what it came from, and how
// many files it holds.
func batchLabel(b files.Batch) string {
	what := b.Label
	if what == "" {
		what = b.Host
	}
	if what == "" {
		what = b.ID
	}
	return fmt.Sprintf("%s (%d)", elideRunes(what, 50), b.Files)
}

// buildRow is one file's line: its path, and what it is.
func (p *filesPane) buildRow(f files.File) *qt.QWidget {
	row := qt.NewQWidget2()
	setName(row.QObject, "FindingRow")
	row.SetAttribute(qt.WA_StyledBackground)

	h := qt.NewQHBoxLayout(row)
	h.SetContentsMargins(rowPadX, 6, rowPadX-4, 6)
	h.SetSpacing(10)

	text := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(text)
	v.SetContentsMargins(0, 0, 0, 0)
	v.SetSpacing(3)
	name := qt.NewQLabel3(f.Name())
	setName(name.QObject, "PanelTitle")
	name.SetToolTip(f.Path)
	sub := qt.NewQLabel3(fileSubtitle(f))
	setName(sub.QObject, "Muted")
	sub.SetToolTip(f.Path)
	for _, l := range []*qt.QLabel{name, sub} {
		l.SetSizePolicy2(qt.QSizePolicy__Ignored, qt.QSizePolicy__Preferred)
	}
	v.AddWidget(name.QWidget)
	v.AddWidget(sub.QWidget)
	h.AddWidget2(text, 1)

	if f.Kind != "" {
		h.AddWidget(newPill(f.Kind, "").w)
	}
	size := qt.NewQLabel3(fileSize(f.Size))
	setName(size.QObject, "Muted")
	h.AddWidget(size.QWidget)
	return row
}

// fileSubtitle is a file's second line: where it sits, what it is and who
// recovered it.
func fileSubtitle(f files.File) string {
	parts := []string{}
	if dir := f.Dir(); dir != "" {
		parts = append(parts, dir)
	}
	parts = append(parts, f.MediaType, fileSource(f))
	return strings.Join(parts, " · ")
}

// fileSource says who recovered a file, naming the plugin or agent session
// when there is one.
func fileSource(f files.File) string {
	switch f.Source {
	case files.SourceAgent:
		if f.Author != "" {
			return "Agent: " + f.Author
		}
		return "Agent"
	case files.SourcePlugin:
		if f.Author != "" {
			return "Plugin: " + f.Author
		}
		return "Plugin"
	}
	return "You"
}

// fileSize is a size for a row: bytes, kibibytes or mebibytes.
func fileSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.0f KiB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1024*1024))
	}
}

func (p *filesPane) selectRow(row int) {
	if row < 0 || row >= len(p.shown) {
		return
	}
	p.current = p.shown[row]
}

// openCurrent opens the detail pane on the selected file, which is what
// reads its bytes from disk.
func (p *filesPane) openCurrent() {
	f, ok := p.store.Get(p.current)
	if !ok {
		return
	}
	p.detail.show(f)
	p.setDetailOpen(true)
}

// read is how the detail pane gets a file's text: from disk, bounded, so a
// recovered bundle can't take the window with it.
func (p *filesPane) read(f files.File) (string, bool, error) {
	data, err := p.store.Read(f.ID, maxFileView+1)
	if err != nil {
		return "", false, err
	}
	truncated := len(data) > maxFileView
	// elide.Prefix counts bytes but never splits a rune: a recovered file is
	// arbitrary bytes, and half a rune would draw as a replacement character.
	return elide.Prefix(string(data), maxFileView), truncated, nil
}

// maxFileView is how much of a file the detail pane shows. Past it the
// editor would be slow and nobody would read the rest on screen anyway; the
// file itself is on disk, and the pane says where.
const maxFileView = 1 << 20 // 1 MiB

func (p *filesPane) showFlow(flowID uint64) {
	if p.onShowFlow != nil {
		p.onShowFlow(flowID)
	}
}

// revealPath selects and opens the recovered file at a path (relative to the
// recovered-files root), clearing any batch or search filter that would hide
// it first. It is how a finding's recovered-file link brings the file into
// view. It returns false when no such file is recorded.
func (p *filesPane) revealPath(path string) bool {
	f, ok := p.store.ByPath(path)
	if !ok {
		return false
	}
	p.batch = ""
	if p.search != nil {
		p.search.SetText("")
	}
	if p.textOnly != nil {
		p.textOnly.SetChecked(false)
	}
	p.current = f.ID
	p.reload()
	p.openCurrent()
	return true
}

// removeFile deletes one file, after asking: the bytes go with the record.
func (p *filesPane) removeFile(id uint64) {
	f, ok := p.store.Get(id)
	if !ok {
		return
	}
	answer := qt.QMessageBox_Question6(p.root, "Delete recovered file",
		"Delete \""+f.Path+"\"?\n\nThe file is removed from disk as well as from the engagement's "+
			"record. This can't be undone.",
		qt.QMessageBox__Yes|qt.QMessageBox__Cancel, qt.QMessageBox__Cancel)
	if answer != qt.QMessageBox__Yes {
		return
	}
	p.current = 0
	p.setDetailOpen(false)
	p.report(p.store.Remove(id))
}

// clearFiles deletes every recovered file, after asking. Anything the app
// never recorded is left in the folder.
func (p *filesPane) clearFiles() {
	n := p.store.Len()
	if n == 0 {
		return
	}
	ok, _ := confirm(p.root, confirmOptions{
		title:   "Delete recovered files",
		heading: fmt.Sprintf("Delete all %d recovered file(s)?", n),
		body: []string{"Their records and the files themselves are removed from disk. Anything else " +
			"in the engagement's recovered folder is left alone.", "This can't be undone."},
		cancel: "Cancel", ok: "Delete",
	})
	if !ok {
		return
	}
	p.current = 0
	p.setDetailOpen(false)
	p.report(p.store.Clear())
}

func (p *filesPane) report(err error) {
	if err != nil {
		qt.QMessageBox_Warning(p.root, "Couldn't change the recovered files", sentence(err.Error()))
	}
}

// ---- The sliding detail pane --------------------------------------------

// setDetailOpen slides the detail pane out or away, as the Findings
// section's does (findingsPane.setDetailOpen).
func (p *filesPane) setDetailOpen(open bool) {
	if open == p.detailOpen {
		return
	}
	p.detailOpen = open

	avail := p.split.Width() - p.split.HandleWidth()
	from, to := 0, 0
	if open {
		to = p.detailWidth
		if to <= 0 {
			to = avail * fileListShare / 100
		}
		to = max(min(to, avail-240), min(460, avail/2))
		p.detailPanel.SetMinimumWidth(0) // so it can start from nothing
		p.sizeDetail(0)
		p.detailPanel.Show()
	} else {
		from = p.currentDetailWidth()
		p.detailWidth = from
		p.detailPanel.SetMinimumWidth(0)
	}
	p.anim.Stop()
	p.anim.SetStartValue(qt.NewQVariant4(from))
	p.anim.SetEndValue(qt.NewQVariant4(to))
	p.anim.Start()
}

func (p *filesPane) sizeDetail(width int) {
	avail := p.split.Width() - p.split.HandleWidth()
	p.split.SetSizes([]int{max(avail-width, 0), width})
}

func (p *filesPane) currentDetailWidth() int {
	sizes := p.split.Sizes()
	if len(sizes) < 2 {
		return 0
	}
	return sizes[1]
}

func (p *filesPane) detailSettled() {
	if p.detailOpen {
		p.detailPanel.SetMinimumWidth(340)
		return
	}
	p.detailPanel.Hide()
	p.sizeDetail(0)
}

func (p *filesPane) refreshTheme() {
	p.reload()
	p.detail.refreshTheme()
}

// ---- The detail pane ----------------------------------------------------

// fileDetailActions are the pane's buttons, wired by the Files pane.
type fileDetailActions struct {
	onClose    func()
	onRemove   func(id uint64)
	onShowFlow func(flowID uint64)
	// read returns a file's text, whether it was cut short, or why it
	// couldn't be read.
	read func(files.File) (text string, truncated bool, err error)
	// root is the directory the files are written under, for the line that
	// says where the file is.
	root func() string
}

// fileDetail is the pane that slides out of the right of the Files section:
// where one recovered file came from, and the file itself.
type fileDetail struct {
	root    *qt.QWidget
	actions fileDetailActions
	shown   uint64

	title   *qt.QLabel
	about   *qt.QLabel
	where   *qt.QVBoxLayout
	content *codeEditor
	meta    *qt.QLabel
	flowID  uint64
	showBtn *qt.QPushButton
}

func newFileDetail(actions fileDetailActions) *fileDetail {
	d := &fileDetail{actions: actions}

	panel := qt.NewQFrame2()
	setName(panel.QObject, "Panel")
	d.root = panel.QWidget
	outer := qt.NewQVBoxLayout(d.root)
	outer.SetContentsMargins(1, 1, 1, 1)
	outer.SetSpacing(0)
	outer.AddWidget(d.buildHeader())

	body := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(body)
	v.SetContentsMargins(rowPadX, 12, rowPadX, 12)
	v.SetSpacing(14)

	d.about = qt.NewQLabel2()
	setName(d.about.QObject, "Muted")
	d.about.SetWordWrap(true)
	v.AddWidget(d.about.QWidget)

	whereHost := qt.NewQWidget2()
	d.where = qt.NewQVBoxLayout(whereHost)
	d.where.SetContentsMargins(0, 0, 0, 0)
	d.where.SetSpacing(4)
	v.AddWidget(section("Where it came from", whereHost))

	// The file reads in the same editor as a message body, with the
	// language picker, so recovered source gets the editor's highlighting
	// and search rather than a plain text box of its own.
	d.content = newCodeEditor(editorOptions{title: "File", languageSelector: true})
	d.content.setReadOnly(true)
	d.content.edit.SetMinimumHeight(260)
	v.AddWidget2(section("", d.content.root), 1)

	d.meta = qt.NewQLabel2()
	setName(d.meta.QObject, "Muted")
	d.meta.SetWordWrap(true)
	v.AddWidget(d.meta.QWidget)

	outer.AddWidget2(body, 1)
	return d
}

func (d *fileDetail) buildHeader() *qt.QWidget {
	header := qt.NewQWidget2()
	setName(header.QObject, "PanelHeader")
	header.SetAttribute(qt.WA_StyledBackground)
	h := qt.NewQHBoxLayout(header)
	h.SetContentsMargins(rowPadX, 6, rowPadX-4, 6)
	h.SetSpacing(8)

	d.title = qt.NewQLabel2()
	setName(d.title.QObject, "PanelTitle")
	d.title.SetSizePolicy2(qt.QSizePolicy__Ignored, qt.QSizePolicy__Preferred)

	d.showBtn = qt.NewQPushButton3("Show request")
	d.showBtn.SetAutoDefault(false)
	d.showBtn.SetToolTip("Show the request this file was recovered from, in Traffic")
	d.showBtn.OnClicked(func() {
		if d.actions.onShowFlow != nil && d.flowID != 0 {
			d.actions.onShowFlow(d.flowID)
		}
	})
	remove := qt.NewQPushButton3("Delete")
	remove.SetAutoDefault(false)
	remove.SetToolTip("Delete this file from the engagement and from disk")
	remove.OnClicked(func() {
		if d.actions.onRemove != nil && d.shown != 0 {
			d.actions.onRemove(d.shown)
		}
	})
	closeBtn := newIconButton(iconClose, "Close (Esc in the list)", func() {
		if d.actions.onClose != nil {
			d.actions.onClose()
		}
	})

	h.AddWidget2(d.title.QWidget, 1)
	h.AddWidget(d.showBtn.QWidget)
	h.AddWidget(remove.QWidget)
	h.AddWidget(closeBtn.w)
	header.SetFixedHeight(remove.SizeHint().Height() + 16)
	return header
}

// show fills the pane from a file, reading its bytes from disk.
func (d *fileDetail) show(f files.File) {
	d.shown, d.flowID = f.ID, f.FlowID
	d.title.SetText(f.Name())
	d.title.SetToolTip(f.Path)
	d.showBtn.SetVisible(f.FlowID != 0)

	about := []string{f.MediaType, fileSize(f.Size), "recovered by " + fileSource(f)}
	if f.Kind != "" {
		about = append(about, f.Kind)
	}
	d.about.SetText(strings.Join(about, " · "))

	clearLayout(d.where.QLayout)
	if f.URL != "" {
		d.where.AddWidget(fileWhereRow(strings.TrimSpace(f.Method + " " + f.URL)))
	}
	if f.Entity != "" {
		d.where.AddWidget(fileWhereRow("Explorer: " + f.Entity))
	}
	if f.BatchLabel != "" && f.BatchLabel != f.URL {
		d.where.AddWidget(fileWhereRow("Recovery: " + f.BatchLabel))
	}
	if root := d.rootDir(); root != "" {
		d.where.AddWidget(fileWhereRow(filepath.Join(root, filepath.FromSlash(f.Path))))
	}
	if f.Notes != "" {
		d.where.AddWidget(fileWhereRow(f.Notes))
	}

	d.content.setTitle(f.Path)
	switch {
	case !f.Text():
		d.content.load(fmt.Sprintf("%s is %s, so it isn't shown here.\n\nIt is on disk, unchanged, "+
			"where the path above says.", f.Name(), f.MediaType), "text/plain")
	default:
		text, truncated, err := d.readFile(f)
		switch {
		case err != nil:
			d.content.load("This file couldn't be read: "+sentence(err.Error()), "text/plain")
		case truncated:
			d.content.load(text, f.MediaType)
			d.content.setTitle(f.Path + " (first " + fileSize(maxFileView) + ")")
		default:
			d.content.load(text, f.MediaType)
		}
	}

	d.meta.SetText(fmt.Sprintf("Recovered at %s · sha256 %s",
		f.Created.Format("15:04:05 on 2 Jan 2006"), f.SHA256))
}

func (d *fileDetail) readFile(f files.File) (string, bool, error) {
	if d.actions.read == nil {
		return "", false, nil
	}
	return d.actions.read(f)
}

func (d *fileDetail) rootDir() string {
	if d.actions.root == nil {
		return ""
	}
	return d.actions.root()
}

// fileWhereRow is one line of the "Where it came from" section.
func fileWhereRow(text string) *qt.QWidget {
	l := qt.NewQLabel3(text)
	setName(l.QObject, "Muted")
	l.SetToolTip(text)
	l.SetWordWrap(true)
	l.SetTextInteractionFlags(qt.TextSelectableByMouse)
	return l.QWidget
}

func (d *fileDetail) refreshTheme() { d.content.refreshTheme() }
