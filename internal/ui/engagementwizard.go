package ui

import (
	"fmt"
	"path/filepath"
	"strings"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/engagement"
	"reproxy/internal/target"
)

// The New Engagement wizard, from File → New Engagement. An engagement is
// one piece of authorised work and the directory that holds everything
// collected under it, so creating one asks for a name, where to keep it, and
// the targets to start from (see internal/engagement).

// engagementPlan is what the wizard collects, ready for
// engagement.Manager.Create.
type engagementPlan struct {
	name        string
	description string
	dir         string
	targets     []target.Target
}

// wizardStep is one page of the wizard.
type wizardStep struct {
	title string
	help  string
	page  *qt.QWidget
	// validate reports why the user can't move on yet, or nil.
	validate func() error
}

// runNewEngagementWizard walks the user through creating an engagement and
// calls create with the result. create returning an error (the folder is in
// use, say) leaves the wizard open with the message shown, so the user can
// adjust rather than start again. It reports whether an engagement was
// created.
func runNewEngagementWizard(parent *qt.QWidget, dataDir string, create func(engagementPlan) error) bool {
	dlg := qt.NewQDialog(parent)
	dlg.SetWindowTitle("New engagement")
	dlg.Resize(720, 600)
	dlg.SetMinimumSize2(600, 480)

	root := qt.NewQVBoxLayout(dlg.QWidget)
	root.SetContentsMargins(0, 0, 0, 0)
	root.SetSpacing(0)

	// ---- Header: which step we are on --------------------------------------
	header := qt.NewQWidget2()
	setName(header.QObject, "PanelHeader")
	header.SetAttribute(qt.WA_StyledBackground)
	hv := qt.NewQVBoxLayout(header)
	hv.SetContentsMargins(22, 14, 22, 14)
	hv.SetSpacing(4)
	stepLabel := qt.NewQLabel2()
	setName(stepLabel.QObject, "Muted")
	stepTitle := qt.NewQLabel2()
	setName(stepTitle.QObject, "PanelTitle")
	stepHelp := helpText("")
	hv.AddWidget(stepLabel.QWidget)
	hv.AddWidget(stepTitle.QWidget)
	hv.AddWidget(stepHelp.QWidget)
	root.AddWidget(header)

	stack := qt.NewQStackedWidget2()
	root.AddWidget2(stack.QWidget, 1)

	// ---- Step 1: name and description --------------------------------------
	name := qt.NewQLineEdit2()
	name.SetPlaceholderText("Acme Q4 web assessment")
	name.SetMinimumWidth(320)
	description := newCodeEditor(editorOptions{title: "Description"})
	description.root.SetMinimumHeight(110)
	description.root.SetMaximumHeight(170)

	namePage := newWizardPage(func(v *qt.QVBoxLayout) {
		form := newSettingsForm()
		form.AddRow3("Name", name.QWidget)
		v.AddLayout(form.QLayout)
		v.AddSpacing(6)
		v.AddWidget(sectionTitle("Description"))
		v.AddWidget(helpText("Optional: the scope of the work, who authorised it, " +
			"anything worth keeping with the engagement.").QWidget)
		v.AddWidget(description.root)
	})

	// ---- Step 2: where it lives ---------------------------------------------
	location := qt.NewQLineEdit2()
	location.SetMinimumWidth(380)
	// The suggested folder follows the name until the user edits it
	// themselves, after which their choice stands.
	locationChosen := false
	name.OnTextEdited(func(text string) {
		if !locationChosen {
			location.SetText(engagement.DefaultDir(dataDir, text))
		}
	})
	location.OnTextEdited(func(string) { locationChosen = true })
	browse := qt.NewQPushButton3("Browse…")
	browse.SetAutoDefault(false)
	browse.OnClicked(func() {
		start := strings.TrimSpace(location.Text())
		if start == "" {
			start = dataDir
		}
		// The chooser only offers existing folders, so start from the parent
		// of the suggestion: the engagement's own folder doesn't exist yet.
		picked := qt.QFileDialog_GetExistingDirectory3(dlg.QWidget,
			"Where to keep this engagement", filepath.Dir(start))
		if picked == "" {
			return
		}
		locationChosen = true
		// A folder picked by hand is the parent unless it is empty, so the
		// engagement still gets a folder of its own named after it.
		location.SetText(filepath.Join(picked, engagement.Slug(name.Text())))
	})

	locationPage := newWizardPage(func(v *qt.QVBoxLayout) {
		row := qt.NewQHBoxLayout2()
		row.SetSpacing(8)
		row.AddWidget2(location.QWidget, 1)
		row.AddWidget(browse.QWidget)
		v.AddWidget(sectionTitle("Folder"))
		v.AddLayout(row.QLayout)
		v.AddWidget(helpText("Everything belonging to this engagement is kept here: its " +
			"targets today, and its captured traffic and findings as those become part of a " +
			"saved engagement. The folder must be new or empty — copy or archive it to share " +
			"the engagement, and open it again from File → Open Engagement.").QWidget)
	})

	// ---- Step 3: settings (nothing yet) ------------------------------------
	settingsPage := newWizardPage(func(v *qt.QVBoxLayout) {
		v.AddWidget(newEmptyState("No engagement settings yet",
			"Settings that apply to one engagement rather than the whole app will live "+
				"here. For now there is nothing to set up."))
	})

	// ---- Step 4: initial targets -------------------------------------------
	targets := newTargetListEditor()
	targetsPage := newWizardPage(func(v *qt.QVBoxLayout) {
		v.AddWidget(targets.root)
		v.AddWidget(helpText("Each target is one domain, IP address or CIDR range. The kind " +
			"follows what you type. Targets can be added, renamed and given path rules " +
			"afterwards from the Targets section.").QWidget)
	})

	steps := []wizardStep{
		{
			title: "Name the engagement",
			help:  "What this piece of work is called.",
			page:  namePage,
			validate: func() error {
				if strings.TrimSpace(name.Text()) == "" {
					return fmt.Errorf("the engagement needs a name")
				}
				return nil
			},
		},
		{
			title: "Choose where to keep it",
			help:  "A folder on this machine that holds the engagement's data.",
			page:  locationPage,
			validate: func() error {
				if strings.TrimSpace(location.Text()) == "" {
					return fmt.Errorf("choose where to store the engagement")
				}
				return nil
			},
		},
		{
			title: "Settings",
			help:  "Per-engagement settings.",
			page:  settingsPage,
		},
		{
			title: "Add the first targets",
			help:  "The hosts in scope. You can change these later.",
			page:  targetsPage,
		},
	}
	for _, s := range steps {
		stack.AddWidget(s.page)
	}

	// ---- Footer: the error, then the buttons -------------------------------
	footer := qt.NewQWidget2()
	setName(footer.QObject, "SettingsFooter")
	footer.SetAttribute(qt.WA_StyledBackground)
	f := qt.NewQHBoxLayout(footer)
	f.SetContentsMargins(16, 10, 16, 10)
	f.SetSpacing(8)
	errLabel := qt.NewQLabel2()
	setName(errLabel.QObject, "ErrorText")
	errLabel.SetWordWrap(true)
	cancel := qt.NewQPushButton3("Cancel")
	cancel.SetAutoDefault(false)
	back := qt.NewQPushButton3("Back")
	back.SetAutoDefault(false)
	next := qt.NewQPushButton3("Next")
	next.SetDefault(true)
	setName(next.QObject, "PrimaryButton")
	f.AddWidget2(errLabel.QWidget, 1)
	f.AddWidget(cancel.QWidget)
	f.AddWidget(back.QWidget)
	f.AddWidget(next.QWidget)
	root.AddWidget(footer)

	step := 0
	last := len(steps) - 1
	show := func() {
		stack.SetCurrentIndex(step)
		stepLabel.SetText(fmt.Sprintf("Step %d of %d", step+1, len(steps)))
		stepTitle.SetText(steps[step].title)
		stepHelp.SetText(steps[step].help)
		errLabel.SetText("")
		back.SetEnabled(step > 0)
		if step == last {
			next.SetText("Create engagement")
		} else {
			next.SetText("Next")
		}
		// The first field of the step the user lands on takes focus.
		if step == 0 {
			name.SetFocus()
		} else if step == 1 {
			location.SetFocus()
		}
	}

	created := false
	next.OnClicked(func() {
		if v := steps[step].validate; v != nil {
			if err := v(); err != nil {
				errLabel.SetText(sentence(err.Error()))
				return
			}
		}
		if step < last {
			step++
			show()
			return
		}
		plan := engagementPlan{
			name:        strings.TrimSpace(name.Text()),
			description: strings.TrimSpace(description.edit.ToPlainText()),
			dir:         strings.TrimSpace(location.Text()),
			targets:     targets.targets(),
		}
		if err := create(plan); err != nil {
			errLabel.SetText(sentence(err.Error()))
			return
		}
		created = true
		dlg.Accept()
	})
	back.OnClicked(func() {
		if step > 0 {
			step--
			show()
		}
	})
	cancel.OnClicked(dlg.Reject)

	// A name typed first seeds the folder, including on the very first keypress.
	location.SetText(engagement.DefaultDir(dataDir, ""))
	show()
	dlg.Exec()
	dlg.DeleteLater()
	return created
}

// newWizardPage wraps one step's content in the scrolling body every
// settings-style page uses.
func newWizardPage(fill func(v *qt.QVBoxLayout)) *qt.QWidget {
	body := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(body)
	v.SetContentsMargins(22, 18, 22, 18)
	v.SetSpacing(10)
	fill(v)
	v.AddStretch()

	scroll := qt.NewQScrollArea2()
	setName(scroll.QObject, "SettingsPage")
	scroll.SetWidgetResizable(true)
	scroll.SetFrameShape(qt.QFrame__NoFrame)
	scroll.SetWidget(body)
	return scroll.QWidget
}
