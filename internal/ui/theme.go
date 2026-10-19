package ui

import (
	_ "embed"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	qt "github.com/mappu/miqt/qt6"
)

//go:embed assets/theme.qss
var themeQSS string

// themeKind identifies a colour theme. The value is what is stored in the
// config (config.Config.Theme).
type themeKind string

const (
	themeLight  themeKind = "light"
	themeDark   themeKind = "dark"
	themeMatrix themeKind = "matrix"
)

// themeOption is a selectable theme with its display name.
type themeOption struct {
	kind themeKind
	name string
}

// themeOptions is the order shown in Settings → General.
var themeOptions = []themeOption{
	{themeLight, "Light"},
	{themeDark, "Dark"},
	{themeMatrix, "Matrix"},
}

// themeKindFor maps a stored value to a theme, defaulting to light.
func themeKindFor(stored string) themeKind {
	switch themeKind(stored) {
	case themeDark:
		return themeDark
	case themeMatrix:
		return themeMatrix
	default:
		return themeLight
	}
}

// Palette keys. The painted widgets refer to colours by these keys (not by
// hex), so the active theme decides the actual colour; the stylesheet
// template uses the same keys. Keeping the names semantic lets the three
// palettes below stay the single source of truth.
const (
	colorText         = "text"
	colorMuted        = "muted"
	colorDivider      = "divider"
	colorAccent       = "accent"
	colorSelected     = "selected"
	colorHover        = "hover"
	colorSuccess      = "success"
	colorRedirect     = "redirect"
	colorClientError  = "clientError"
	colorServerError  = "serverError"
	colorHeld         = "held"
	colorGutterBg     = "gutterBg"
	colorGutterText   = "gutterText"
	colorCurrentLine  = "currentLine"
	colorMethodGet    = "methodGet"
	colorMethodPost   = "methodPost"
	colorMethodPut    = "methodPut"
	colorMethodDelete = "methodDelete"
	colorMethodOther  = "methodOther"
	// The Entity Explorer's types, each with a colour of its own.
	colorEntityDomain = "entityDomain"
	colorEntityHost   = "entityHost"
	colorEntityURL    = "entityURL"

	synKeyword = "synKeyword"
	synString  = "synString"
	synNumber  = "synNumber"
	synComment = "synComment"
	synTag     = "synTag"
	synAttr    = "synAttr"
	synKey     = "synKey"
	synLiteral = "synLiteral"
)

type palette = map[string]string

// lightPalette is the original look; dark and matrix map the same keys to a
// dark surface. Every palette must define every key the stylesheet and the
// painted widgets use.
var lightPalette = palette{
	// The sixteen colours a terminal names rather than gives: what the
	// shell means by "blue" is this theme's blue, so prompts and ls stay
	// readable whichever theme is on (see terminalANSI).
	"ansiBlack":         "#1f2328",
	"ansiRed":           "#c62828",
	"ansiGreen":         "#1a7f37",
	"ansiYellow":        "#b45309",
	"ansiBlue":          "#3b6ef5",
	"ansiMagenta":       "#8250df",
	"ansiCyan":          "#0f8b8d",
	"ansiWhite":         "#6b7280",
	"ansiBrightBlack":   "#4b5563",
	"ansiBrightRed":     "#e05252",
	"ansiBrightGreen":   "#22a04a",
	"ansiBrightYellow":  "#d08700",
	"ansiBrightBlue":    "#2f5ee0",
	"ansiBrightMagenta": "#a371f7",
	"ansiBrightCyan":    "#1aa7a9",
	"ansiBrightWhite":   "#a3aab3",
	"entityDomain":      "#8250df",
	"entityHost":        "#0f8b8d",
	"entityURL":         "#bf3989",
	"text":              "#1f2328",
	"textSubtle":        "#4b5563",
	"muted":             "#6b7280",
	"textDisabled":      "#a3aab3",
	"windowBg":          "#f6f7f9",
	"surface":           "#ffffff",
	"surfaceAlt":        "#fbfcfd",
	"hover":             "#f3f5f8",
	"hoverStrong":       "#eef1f5",
	"border":            "#e4e7eb",
	"borderSubtle":      "#eceef1",
	"inputBorder":       "#d5d9df",
	"divider":           "#eef0f3",
	"accent":            "#3b6ef5",
	"accentHover":       "#2f5ee0",
	"onAccent":          "#ffffff",
	"selected":          "#eaf0ff",
	"selectionBg":       "#cfdcff",
	"success":           "#1a7f37",
	"redirect":          "#3b6ef5",
	"clientError":       "#b45309",
	"serverError":       "#c62828",
	"held":              "#b45309",
	"dangerBg":          "#fdecec",
	"warnText":          "#92400e",
	"warnBg":            "#fff1d6",
	"unviewedBg":        "#fff8e6",
	"warnBorder":        "#f5d9a8",
	"headerKey":         "#6b3fa0",
	"chipBg":            "#f0f2f5",
	"gutterBg":          "#f6f7f9",
	"gutterText":        "#b0b6bd",
	"currentLine":       "#f3f6fb",
	"scrollHandle":      "#d5d9df",
	"scrollHover":       "#b8bec7",
	"tooltipBg":         "#1f2328",
	"tooltipText":       "#ffffff",
	"methodGet":         "#3b6ef5",
	"methodPost":        "#1a7f37",
	"methodPut":         "#b45309",
	"methodDelete":      "#c62828",
	"methodOther":       "#6b7280",
	"synKeyword":        "#0000ff",
	"synString":         "#a31515",
	"synNumber":         "#098658",
	"synComment":        "#008000",
	"synTag":            "#800000",
	"synAttr":           "#e50000",
	"synKey":            "#0451a5",
	"synLiteral":        "#0000ff",
}

var darkPalette = palette{
	// See the light palette: named colours belong to the theme, and on a
	// dark surface they have to be light enough to read — the dark blue a
	// shell paints paths in is what made this necessary.
	"ansiBlack":         "#3a414c",
	"ansiRed":           "#f06d6d",
	"ansiGreen":         "#3fb950",
	"ansiYellow":        "#e3a008",
	"ansiBlue":          "#4f8cff",
	"ansiMagenta":       "#b392f0",
	"ansiCyan":          "#39c5bb",
	"ansiWhite":         "#c8cfd8",
	"ansiBrightBlack":   "#5b6673",
	"ansiBrightRed":     "#ff8f8f",
	"ansiBrightGreen":   "#56d364",
	"ansiBrightYellow":  "#f0b429",
	"ansiBrightBlue":    "#7cb0ff",
	"ansiBrightMagenta": "#c9a9ff",
	"ansiBrightCyan":    "#5fd9d0",
	"ansiBrightWhite":   "#ffffff",
	"entityDomain":      "#b392f0",
	"entityHost":        "#39c5bb",
	"entityURL":         "#f778ba",
	"text":              "#e6e9ee",
	"textSubtle":        "#aab2bd",
	"muted":             "#8b95a3",
	"textDisabled":      "#5b6673",
	"windowBg":          "#1e2127",
	"surface":           "#252a31",
	"surfaceAlt":        "#2b313a",
	"hover":             "#2f3640",
	"hoverStrong":       "#343b45",
	"border":            "#3a414c",
	"borderSubtle":      "#30363f",
	"inputBorder":       "#3a414c",
	"divider":           "#2a2f37",
	"accent":            "#4f8cff",
	"accentHover":       "#3f7af0",
	"onAccent":          "#ffffff",
	"selected":          "#2b3a57",
	"selectionBg":       "#35507e",
	"success":           "#3fb950",
	"redirect":          "#4f8cff",
	"clientError":       "#e3a008",
	"serverError":       "#f06d6d",
	"held":              "#e3a008",
	"dangerBg":          "#3a2526",
	"warnText":          "#e3b341",
	"warnBg":            "#3a2f18",
	"unviewedBg":        "#2f2a1a",
	"warnBorder":        "#5a4a22",
	"headerKey":         "#c58fff",
	"chipBg":            "#2f3640",
	"gutterBg":          "#22272e",
	"gutterText":        "#596170",
	"currentLine":       "#262c34",
	"scrollHandle":      "#3a414c",
	"scrollHover":       "#4a525f",
	"tooltipBg":         "#0b0e13",
	"tooltipText":       "#e6e9ee",
	"methodGet":         "#4f8cff",
	"methodPost":        "#3fb950",
	"methodPut":         "#e3a008",
	"methodDelete":      "#f06d6d",
	"methodOther":       "#8b95a3",
	"synKeyword":        "#569cd6",
	"synString":         "#ce9178",
	"synNumber":         "#b5cea8",
	"synComment":        "#6a9955",
	"synTag":            "#569cd6",
	"synAttr":           "#9cdcfe",
	"synKey":            "#9cdcfe",
	"synLiteral":        "#569cd6",
}

// matrixPalette is dark with the bright phosphor green of The Matrix. The UI
// chrome is green on near-black; status colours stay distinct (amber/red) so
// 4xx/5xx still stand out.
var matrixPalette = palette{
	// Named colours in phosphor: the hues stay inside the theme (greens and
	// mints, amber and red for what a terminal paints as warnings), so a
	// coloured prompt still looks like this screen.
	"ansiBlack":         "#146b2d",
	"ansiRed":           "#ff5555",
	"ansiGreen":         "#39ff14",
	"ansiYellow":        "#f5c451",
	"ansiBlue":          "#8affc0",
	"ansiMagenta":       "#a6ff8f",
	"ansiCyan":          "#27c552",
	"ansiWhite":         "#33ff66",
	"ansiBrightBlack":   "#1f9f44",
	"ansiBrightRed":     "#ff8080",
	"ansiBrightGreen":   "#7dff5a",
	"ansiBrightYellow":  "#ffd98a",
	"ansiBrightBlue":    "#b6ffdb",
	"ansiBrightMagenta": "#c8ffb8",
	"ansiBrightCyan":    "#5bff40",
	"ansiBrightWhite":   "#ccffd9",
	"entityDomain":      "#a6ff8f",
	"entityHost":        "#8affc0",
	"entityURL":         "#f5c451",
	"text":              "#33ff66",
	"textSubtle":        "#27c552",
	"muted":             "#1f9f44",
	"textDisabled":      "#146b2d",
	"windowBg":          "#020403",
	"surface":           "#0a0f0b",
	"surfaceAlt":        "#0d140e",
	"hover":             "#10301a",
	"hoverStrong":       "#133a20",
	"border":            "#124a22",
	"borderSubtle":      "#0d2c16",
	"inputBorder":       "#1a6b30",
	"divider":           "#0d2c16",
	"accent":            "#39ff14",
	"accentHover":       "#5bff40",
	"onAccent":          "#001b06",
	"selected":          "#0f3a1c",
	"selectionBg":       "#1f7a38",
	"success":           "#39ff14",
	"redirect":          "#8affc0",
	"clientError":       "#f5c451",
	"serverError":       "#ff5555",
	"held":              "#f5c451",
	"dangerBg":          "#2a0d0d",
	"warnText":          "#f5c451",
	"warnBg":            "#241a06",
	"unviewedBg":        "#122a14",
	"warnBorder":        "#5a4a16",
	"headerKey":         "#a6ff8f",
	"chipBg":            "#0f2a16",
	"gutterBg":          "#050a06",
	"gutterText":        "#166b2d",
	"currentLine":       "#0b1f0f",
	"scrollHandle":      "#1a6b30",
	"scrollHover":       "#27c552",
	"tooltipBg":         "#001206",
	"tooltipText":       "#39ff14",
	"methodGet":         "#39ff14",
	"methodPost":        "#7dff5a",
	"methodPut":         "#f5c451",
	"methodDelete":      "#ff5555",
	"methodOther":       "#1f9f44",
	"synKeyword":        "#39ff14",
	"synString":         "#a6ff8f",
	"synNumber":         "#7dff5a",
	"synComment":        "#1f9f44",
	"synTag":            "#39ff14",
	"synAttr":           "#7dff5a",
	"synKey":            "#8affc0",
	"synLiteral":        "#5bff40",
}

var palettes = map[themeKind]palette{
	themeLight:  lightPalette,
	themeDark:   darkPalette,
	themeMatrix: matrixPalette,
}

// activePalette is the palette the painted widgets and the stylesheet read.
var activePalette = lightPalette

// color returns the active theme's colour for a palette key. A literal hex
// string (not a key) is returned as-is, so callers may still pass one.
func color(key string) *qt.QColor { return qt.NewQColor6(hex(key)) }

// hex resolves a palette key to its hex string for the active theme, or
// returns key unchanged when it isn't a key (e.g. a literal hex).
func hex(key string) string {
	if v, ok := activePalette[key]; ok {
		return v
	}
	return key
}

// preferredUIFonts are tried in order; the platform default is used if none
// is installed (Segoe UI and SF Pro, the Windows/macOS defaults, are fine).
var preferredUIFonts = []string{"Inter", "Inter Variable"}

var qssTemplate = template.Must(template.New("qss").Parse(themeQSS))

// Theme application state, kept so the theme can be switched live.
var (
	themeApp *qt.QApplication
	themeDir string
	arrowSeq int
)

// initTheme sets the Fusion style and font once, then applies kind. The
// returned function removes the theme's temporary files; call it on exit.
func initTheme(app *qt.QApplication, kind themeKind) (cleanup func()) {
	themeApp = app
	qt.QApplication_SetStyleWithStyle("Fusion")

	font := qt.QApplication_Font()
	for _, family := range preferredUIFonts {
		if qt.QFontDatabase_HasFamily(family) {
			font.SetFamily(family)
			break
		}
	}
	if font.PointSizeF() < 10 {
		font.SetPointSizeF(10)
	}
	qt.QApplication_SetFont(font)

	var err error
	themeDir, err = os.MkdirTemp("", "reproxy-theme-")
	if err != nil {
		themeDir = "" // only costs the combo-box arrow
	}
	setTheme(kind)
	return func() {
		if themeDir != "" {
			os.RemoveAll(themeDir)
		}
	}
}

// setTheme switches the active palette and re-applies the stylesheet and the
// base QPalette. Painted widgets pick up the change on their next repaint.
func setTheme(kind themeKind) {
	if p, ok := palettes[kind]; ok {
		activePalette = p
	} else {
		activePalette = lightPalette
	}
	if themeApp == nil {
		return
	}
	applyBasePalette()
	arrowSeq++
	themeApp.SetStyleSheet(renderQSS(writeThemeImages(themeDir, arrowSeq)))
}

// applyBasePalette sets the Fusion palette so widgets and parts the stylesheet
// doesn't reach (checkboxes, text cursors, unstyled areas) match the theme.
func applyBasePalette() {
	pal := qt.NewQPalette()
	set := func(role qt.QPalette__ColorRole, key string) { pal.SetColor2(role, color(key)) }
	set(qt.QPalette__Window, colorWindowBg)
	set(qt.QPalette__WindowText, colorText)
	set(qt.QPalette__Base, "surface")
	set(qt.QPalette__AlternateBase, "surfaceAlt")
	set(qt.QPalette__Text, colorText)
	set(qt.QPalette__Button, "surface")
	set(qt.QPalette__ButtonText, colorText)
	set(qt.QPalette__BrightText, "onAccent")
	set(qt.QPalette__Highlight, colorSelected)
	set(qt.QPalette__HighlightedText, colorText)
	set(qt.QPalette__ToolTipBase, "tooltipBg")
	set(qt.QPalette__ToolTipText, "tooltipText")
	set(qt.QPalette__PlaceholderText, colorMuted)
	set(qt.QPalette__Link, colorAccent)
	pal.SetColor(qt.QPalette__Disabled, qt.QPalette__Text, color("textDisabled"))
	pal.SetColor(qt.QPalette__Disabled, qt.QPalette__WindowText, color("textDisabled"))
	pal.SetColor(qt.QPalette__Disabled, qt.QPalette__ButtonText, color("textDisabled"))
	qt.QApplication_SetPalette(pal)
}

// colorWindowBg is a palette-only key (no painted-widget const needs it).
const colorWindowBg = "windowBg"

// renderQSS fills the stylesheet template with the active palette plus the
// paths of the images painted for it (see writeThemeImages).
func renderQSS(images map[string]string) string {
	data := make(map[string]string, len(activePalette)+len(images))
	for k, v := range activePalette {
		data[k] = v
	}
	for k, v := range images {
		data[k] = v
	}
	var b strings.Builder
	if err := qssTemplate.Execute(&b, data); err != nil {
		return "" // a parse-time template is validated at init; execution won't fail
	}
	return b.String()
}

// writeThemeImages paints the images the stylesheet references into dir and
// returns their paths by template key. A key missing or empty only costs that
// one image. seq makes the filenames unique per theme switch so Qt reloads
// them instead of using cached copies. A 2x copy is written alongside each for
// high-DPI screens.
func writeThemeImages(dir string, seq int) map[string]string {
	images := map[string]string{}
	if dir == "" {
		return images
	}
	add := func(key, name string, paint func(path string, scale int) bool) {
		base := name + "-" + strconv.Itoa(seq)
		path := filepath.Join(dir, base+".png")
		if paint(path, 1) && paint(filepath.Join(dir, base+"@2x.png"), 2) {
			images[key] = filepath.ToSlash(path)
		}
	}
	add("arrowDown", "arrow", paintArrow)
	add("closeIcon", "close", func(path string, scale int) bool {
		return paintClose(path, scale, color(colorMuted))
	})
	add("closeHover", "close-hover", func(path string, scale int) bool {
		return paintClose(path, scale, color(colorServerError))
	})
	return images
}

func paintArrow(path string, scale int) bool {
	const w, h = 10, 6
	pm := qt.NewQPixmap2(w*scale, h*scale)
	pm.FillWithFillColor(qt.NewQColor11(0, 0, 0, 0))
	p := qt.NewQPainter2(pm.QPaintDevice)
	p.SetRenderHint(qt.QPainter__Antialiasing)
	tri := qt.NewQPainterPath()
	s := float64(scale)
	tri.MoveTo2(1*s, 1*s)
	tri.LineTo2(9*s, 1*s)
	tri.LineTo2(5*s, 5.5*s)
	tri.CloseSubpath()
	p.FillPath(tri, qt.NewQBrush3(color(colorMuted)))
	p.End()
	return pm.Save(path)
}

// paintClose draws the × on a tab's close button, which Qt's own style would
// otherwise draw in colours of its own.
func paintClose(path string, scale int, col *qt.QColor) bool {
	const size = 10
	pm := qt.NewQPixmap2(size*scale, size*scale)
	pm.FillWithFillColor(qt.NewQColor11(0, 0, 0, 0))
	p := qt.NewQPainter2(pm.QPaintDevice)
	p.SetRenderHint(qt.QPainter__Antialiasing)
	pen := qt.NewQPen3(col)
	pen.SetWidthF(1.4 * float64(scale))
	pen.SetCapStyle(qt.RoundCap)
	p.SetPenWithPen(pen)
	s, in := float64(scale), 2.6
	p.DrawLine4(qt.NewQPointF3(in*s, in*s), qt.NewQPointF3((size-in)*s, (size-in)*s))
	p.DrawLine4(qt.NewQPointF3((size-in)*s, in*s), qt.NewQPointF3(in*s, (size-in)*s))
	p.End()
	return pm.Save(path)
}

// monospaceFont returns the platform's fixed-width font, used for raw HTTP.
func monospaceFont() *qt.QFont {
	f := qt.QFontDatabase_SystemFont(qt.QFontDatabase__FixedFont)
	f.SetPointSizeF(qt.QApplication_Font().PointSizeF())
	return f
}

// terminalANSI is the palette key for one of a terminal's sixteen named
// colours, 0–15 in the usual order (black, red, green, yellow, blue,
// magenta, cyan, white, then the same eight bright).
func terminalANSI(index uint8) string {
	names := [...]string{
		"ansiBlack", "ansiRed", "ansiGreen", "ansiYellow",
		"ansiBlue", "ansiMagenta", "ansiCyan", "ansiWhite",
		"ansiBrightBlack", "ansiBrightRed", "ansiBrightGreen", "ansiBrightYellow",
		"ansiBrightBlue", "ansiBrightMagenta", "ansiBrightCyan", "ansiBrightWhite",
	}
	return names[index&0x0f]
}

// setName sets the objectName that theme.qss selectors (#Name) match on.
func setName(o *qt.QObject, name string) {
	o.SetObjectName(*qt.NewQAnyStringView3(name))
}
