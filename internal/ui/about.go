package ui

import (
	"fmt"
	"html"
	"runtime"

	qt "github.com/mappu/miqt/qt6"
)

// Version is the build's version, shown in the About box. The CLI sets it
// from its own ldflags-stamped version before calling Run.
var Version = "dev"

// openAbout shows the About box: what reProxy is, which build is running and
// where it keeps its state. It is modal and small; the build details are
// selectable so they can be pasted into a bug report.
func openAbout(parent *qt.QWidget, dataDir string) {
	dlg := qt.NewQDialog(parent)
	dlg.SetAttribute(qt.WA_DeleteOnClose)
	dlg.SetWindowTitle("About " + AppName)

	v := qt.NewQVBoxLayout(dlg.QWidget)
	v.SetContentsMargins(24, 20, 24, 16)
	v.SetSpacing(12)

	head := qt.NewQHBoxLayout2()
	head.SetSpacing(14)
	if icon := qt.QGuiApplication_WindowIcon(); !icon.IsNull() {
		logo := qt.NewQLabel2()
		logo.SetPixmap(icon.Pixmap(qt.NewQSize2(64, 64)))
		head.AddWidget(logo.QWidget)
	}
	names := qt.NewQVBoxLayout2()
	names.SetSpacing(2)
	title := qt.NewQLabel3(AppName)
	setName(title.QObject, "PanelTitle")
	names.AddWidget(title.QWidget)
	ver := qt.NewQLabel3("Version " + Version)
	setName(ver.QObject, "Muted")
	ver.SetTextInteractionFlags(qt.TextSelectableByMouse)
	names.AddWidget(ver.QWidget)
	names.AddStretch()
	head.AddLayout(names.QLayout)
	head.AddStretch()
	v.AddLayout(head.QLayout)

	blurb := qt.NewQLabel3(AppName + " is an intercepting proxy for testing web apps before they " +
		"reach production. It records the traffic your browser sends through it, and lets you " +
		"hold, edit and replay requests and responses — by hand, from plugins, or with an " +
		"LLM agent working alongside you.")
	blurb.SetWordWrap(true)
	v.AddWidget(blurb.QWidget)

	qtVersion := qt.QLibraryInfo_Version().ToString()
	rows := [][2]string{
		{"Built with", fmt.Sprintf("%s, Qt %s", runtime.Version(), qtVersion)},
		{"Platform", runtime.GOOS + "/" + runtime.GOARCH},
		{"Data folder", dataDir},
	}
	details := "<table cellspacing='0' cellpadding='2'>"
	for _, r := range rows {
		details += fmt.Sprintf("<tr><td style='padding-right:12px'><b>%s</b></td><td>%s</td></tr>",
			html.EscapeString(r[0]), html.EscapeString(r[1]))
	}
	details += "</table>"
	info := qt.NewQLabel3(details)
	info.SetTextFormat(qt.RichText)
	info.SetTextInteractionFlags(qt.TextSelectableByMouse)
	v.AddWidget(info.QWidget)

	buttons := qt.NewQDialogButtonBox4(qt.QDialogButtonBox__Close)
	buttons.OnRejected(func() { dlg.Close() })
	v.AddWidget(buttons.QWidget)

	dlg.SetFixedWidth(440)
	dlg.AdjustSize()
	dlg.SetFixedSize(dlg.Size())
	dlg.Exec()
}
