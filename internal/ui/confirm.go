package ui

import (
	"fmt"
	"strings"

	qt "github.com/mappu/miqt/qt6"
)

// confirmOptions describe a confirmation asked before an action that is
// hard to undo or noisy: Autocrawl's start, a bulk promote or suppress.
type confirmOptions struct {
	title   string   // the window's
	heading string   // bold, at the top
	body    []string // paragraphs
	// items, when set, are listed under the body (the first maxConfirmItems
	// of them, then how many more): what the action applies to.
	items []string
	// check, when set, is a checkbox under the text, unticked; confirm
	// reports whether it was ticked.
	check  string
	cancel string // the button that backs out
	ok     string // the one that goes ahead, coloured
}

// maxConfirmItems is how many items a confirmation lists by name.
const maxConfirmItems = 12

// confirm asks the question opts describes, modal to parent's window only,
// so the rest of the app stays usable. ok is true when the user went ahead;
// checked is whether opts.check was ticked.
//
// The text is laid out at a width that fits the screen and each paragraph
// is given the height it needs at that width: Qt sizes a dialog from its
// labels' one-line size hints otherwise, which on a small or scaled screen
// cuts the text and the buttons off.
func confirm(parent *qt.QWidget, opts confirmOptions) (ok, checked bool) {
	dlg := qt.NewQDialog(parent)
	dlg.SetWindowTitle(opts.title)
	dlg.SetWindowModality(qt.WindowModal)

	const margin = 20
	textWidth := 520
	if scr := parent.Screen(); scr != nil {
		textWidth = max(240, min(textWidth, scr.AvailableGeometry().Width()-2*margin-48))
	}

	v := qt.NewQVBoxLayout(dlg.QWidget)
	v.SetContentsMargins(margin, 18, margin, 16)
	v.SetSpacing(12)
	v.SetSizeConstraint(qt.QLayout__SetMinimumSize) // never smaller than its content
	paragraph := func(l *qt.QLabel) {
		l.SetWordWrap(true)
		l.SetFixedWidth(textWidth)
		l.SetMinimumHeight(l.HeightForWidth(textWidth))
		v.AddWidget(l.QWidget)
	}
	heading := qt.NewQLabel3(opts.heading)
	setName(heading.QObject, "SettingsPageTitle")
	paragraph(heading)
	for _, text := range opts.body {
		paragraph(helpText(text))
	}
	if len(opts.items) > 0 {
		shown := opts.items
		if len(shown) > maxConfirmItems {
			shown = shown[:maxConfirmItems]
		}
		lines := make([]string, 0, len(shown)+1)
		for _, it := range shown {
			lines = append(lines, "•  "+it)
		}
		if more := len(opts.items) - len(shown); more > 0 {
			lines = append(lines, fmt.Sprintf("…and %d more", more))
		}
		list := qt.NewQLabel3(strings.Join(lines, "\n"))
		list.SetTextInteractionFlags(qt.TextSelectableByMouse)
		paragraph(list)
	}
	var box *qt.QCheckBox
	if opts.check != "" {
		box = qt.NewQCheckBox3(opts.check)
		v.AddWidget(box.QWidget)
	}

	cancel := qt.NewQPushButton3(opts.cancel)
	// Neither button is the default, so Enter does nothing (nothing happens
	// by accident) and Escape cancels; only the go-ahead is coloured.
	cancel.SetAutoDefault(false)
	cancel.OnClicked(func() { dlg.Reject() })
	proceed := qt.NewQPushButton3(opts.ok)
	setName(proceed.QObject, "PrimaryButton")
	proceed.SetAutoDefault(false)
	proceed.OnClicked(func() { dlg.Accept() })
	for _, b := range []*qt.QPushButton{cancel, proceed} {
		b.SetMinimumSize(b.SizeHint()) // the whole label, always
	}
	// Side by side when they fit beside each other, stacked (the go-ahead
	// on top) when they don't.
	if cancel.SizeHint().Width()+proceed.SizeHint().Width()+12 <= textWidth {
		buttons := qt.NewQHBoxLayout2()
		buttons.AddStretch()
		buttons.AddWidget(cancel.QWidget)
		buttons.AddWidget(proceed.QWidget)
		v.AddLayout(buttons.QLayout)
	} else {
		v.AddWidget(proceed.QWidget)
		v.AddWidget(cancel.QWidget)
	}

	dlg.AdjustSize()
	ok = dlg.Exec() == int(qt.QDialog__Accepted)
	return ok, ok && box != nil && box.IsChecked()
}
