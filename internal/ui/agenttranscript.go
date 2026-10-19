package ui

import (
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/agent"
	"reproxy/internal/findings"
)

// The Agents transcript: a readable chat rendered as HTML in the panel's
// QTextBrowser (see agentspanel.go). It is a document, not a widget tree, so
// everything here is HTML + a CSS default stylesheet; interaction (expanding a
// tool call, opening a replay session) goes through anchor links that
// onAnchor handles.
//
// Three kinds of line:
//   - the user's and the agent's prose, with light inline markdown;
//   - a tool call that created something — a finding, a replay session, an
//     entity — shown as a card with the object type's icon and the basics;
//   - any other tool call (reads, inspects), shown as one quiet line that can
//     be expanded to reveal the raw request and response.
//
// Colours come from the active theme (hex), so the transcript follows a theme
// switch; refreshTheme re-applies the stylesheet and redraws.

// tileColors are the object-type accents a card's icon tile is painted in.
// They are baked into the icon image (a rounded square the white glyph sits
// on), so they stay saturated on every theme and need no regenerating when it
// changes.
const (
	findingTile   = "#d6453d"
	replayTile    = "#8b5cf6"
	entityTile    = "#bf3989"
	sourceMapTile = "#0ea5a4"
)

// setupTranscriptAssets writes the images the transcript references — the
// object-type tiles and the reveal eye — and applies the themed stylesheet.
// Called once when the panel is built; refreshTheme re-applies the stylesheet.
func (p *agentsPanel) setupTranscriptAssets() {
	dir, err := os.MkdirTemp("", "reproxy-agent-icons-")
	if err == nil {
		p.iconDir = dir
		writeAgentIcons(dir)
		writeEyeIcons(dir, hex(colorAccent))
		p.transcript.SetSearchPaths([]string{dir})
	}
	p.transcript.Document().SetDefaultStyleSheet(transcriptCSS())
}

// refreshTheme re-applies the stylesheet in the new theme's colours and
// redraws the open session. The tiles bake their own colours, so only the eye
// (drawn in the accent colour) is regenerated.
func (p *agentsPanel) refreshTheme() {
	if p.iconDir != "" {
		writeEyeIcons(p.iconDir, hex(colorAccent))
	}
	p.transcript.Document().SetDefaultStyleSheet(transcriptCSS())
	if p.current != "" {
		p.rerender()
	}
}

// writeAgentIcons paints each object-type icon as a self-contained square
// tile — a rounded colour swatch with the white glyph centred on it — and
// saves it as a PNG the transcript loads by name. Baking the colour into the
// image means the card's table cell needs no coloured background, so the tile
// stays a square instead of stretching down the card's full height.
func writeAgentIcons(dir string) {
	white := qt.NewQColor6("#ffffff")
	for _, ic := range []struct {
		name string
		kind iconKind
		tile string
	}{
		{"finding", iconFindings, findingTile},
		{"replay", iconReplay, replayTile},
		{"entity", iconMap, entityTile},
		{"sourcemap", iconImport, sourceMapTile},
	} {
		const s = 56 // rendered at 2× and shown at 28, so it stays crisp
		pix := qt.NewQPixmap2(s, s)
		pix.FillWithFillColor(qt.NewQColor2(qt.Transparent))
		pt := qt.NewQPainter2(pix.QPaintDevice)
		pt.SetRenderHint(qt.QPainter__Antialiasing)
		pt.SetPenWithStyle(qt.NoPen)
		pt.SetBrush(qt.NewQBrush3(qt.NewQColor6(ic.tile)))
		pt.DrawRoundedRect(qt.NewQRectF4(0, 0, s, s), 13, 13)
		drawIcon(pt, ic.kind, s*0.24, s*0.24, s*0.52, white)
		pt.End()
		pix.Save2(filepath.Join(dir, ic.name+".png"), "PNG")
	}
}

// writeEyeIcons paints the reveal/hide eye in the given colour (the accent, so
// it reads as a control) to eye.png and eyeoff.png — the control beside a
// hidden finding's masked value.
func writeEyeIcons(dir, colorHex string) {
	col := qt.NewQColor6(colorHex)
	for name, kind := range map[string]iconKind{"eye": iconEye, "eyeoff": iconEyeOff} {
		const s = 30 // rendered at 2× and shown at 15
		pix := qt.NewQPixmap2(s, s)
		pix.FillWithFillColor(qt.NewQColor2(qt.Transparent))
		pt := qt.NewQPainter2(pix.QPaintDevice)
		pt.SetRenderHint(qt.QPainter__Antialiasing)
		drawIcon(pt, kind, 1, 1, s-2, col)
		pt.End()
		pix.Save2(filepath.Join(dir, name+".png"), "PNG")
	}
}

// transcriptCSS is the document stylesheet, in the active theme's colours.
// QTextDocument supports a CSS subset: class selectors and colour/background,
// font and margin properties are reliable; borders and radii are not, so the
// cards are laid out with background-filled tables instead (see toolCardHTML).
func transcriptCSS() string {
	return fmt.Sprintf(`
		p { margin:2px 0 9px 0; color:%[1]s; }
		p.label { color:%[2]s; font-size:8pt; font-weight:bold; margin:12px 0 2px 0; }
		p.user { color:%[1]s; }
		p.sys { color:%[2]s; font-style:italic; }
		a { color:%[3]s; text-decoration:none; }
		code { font-family:'DejaVu Sans Mono',monospace; color:%[4]s; }
		p.tool { color:%[2]s; font-size:9pt; margin:3px 0; }
		.disc { color:%[5]s; font-weight:bold; }
		.ok { color:%[6]s; font-weight:bold; }
		.err { color:%[7]s; font-weight:bold; }
		pre.io { background-color:%[8]s; color:%[4]s; font-family:'DejaVu Sans Mono',monospace;
			font-size:8pt; margin:2px 0 9px 0; }
		span.kind { color:%[2]s; font-size:8pt; font-weight:bold; }
		span.headline { color:%[1]s; font-weight:bold; }
		span.meta { color:%[2]s; font-size:9pt; }
		span.chip { background-color:%[9]s; color:%[2]s; font-size:8pt; }
	`,
		hex(colorText), hex(colorMuted), hex(colorAccent), hex("textSubtle"),
		hex("textDisabled"), hex(colorSuccess), hex(colorServerError), hex("surfaceAlt"), hex("chipBg"))
}

// renderTranscript renders a whole session; renderMessage renders one message,
// keyed by its index i so a tool call's disclosure link can name it.
func (p *agentsPanel) renderTranscript(s agent.Session) string {
	var b strings.Builder
	for i, m := range s.Messages {
		b.WriteString(p.renderMessage(i, m))
	}
	if s.Status == agent.StatusRunning {
		b.WriteString(`<p class="sys">Working…</p>`)
	}
	return b.String()
}

// appendMessage adds one streamed message, with the index it will have in a
// full re-render, so its disclosure link stays correct.
func (p *agentsPanel) appendMessage(m agent.Message) {
	p.transcript.Append(p.renderMessage(p.rendered, m))
	p.rendered++
	p.scrollToEnd()
}

func (p *agentsPanel) renderMessage(i int, m agent.Message) string {
	switch m.Role {
	case agent.RoleUser:
		return fmt.Sprintf(`<p class="label">You</p><p class="user">%s</p>`, mdInline(m.Text))
	case agent.RoleAssistant:
		return fmt.Sprintf(`<p class="label">Agent</p><p>%s</p>`, mdInline(m.Text))
	case agent.RoleTool:
		return p.renderToolCall(i, m.Tool)
	default: // system
		return fmt.Sprintf(`<p class="sys">%s</p>`, mdInline(m.Text))
	}
}

// renderToolCall renders a tool call: a card for one that created an object,
// otherwise a quiet, expandable line. Either way the raw input and output can
// be revealed by the disclosure link.
func (p *agentsPanel) renderToolCall(i int, t *agent.ToolCall) string {
	if t == nil {
		return ""
	}
	open := p.expanded[i]
	revealed := p.revealed[i]
	var b strings.Builder
	if card, ok := p.toolCard(i, t, revealed); ok {
		b.WriteString(card)
		b.WriteString(fmt.Sprintf(`<p class="tool">%s <code>%s</code></p>`, disclosure(i, open), html.EscapeString(t.Name)))
	} else {
		glyph := `<span class="ok">✓</span>`
		if t.IsError {
			glyph = `<span class="err">✕</span>`
		}
		line := fmt.Sprintf(`<p class="tool">%s %s <code>%s</code>`, disclosure(i, open), glyph, html.EscapeString(t.Name))
		if sum := readSummary(t); sum != "" {
			line += " — " + html.EscapeString(sum)
		}
		b.WriteString(line + "</p>")
	}
	if open {
		// A hidden finding's value is kept out of the raw output too, until the
		// eye reveals it — so it isn't one disclosure click from a shoulder.
		b.WriteString(rawIO(t, secretOf(t, revealed)))
	}
	return b.String()
}

// secretOf returns a hidden finding's value, to redact from the raw I/O, or ""
// when there is nothing to hide or it has been revealed.
func secretOf(t *agent.ToolCall, revealed bool) string {
	if revealed {
		return ""
	}
	if f := jmap(parseToolOutput(t.Output)["finding"]); f != nil && jbool(f["hidden"]) {
		return jstr(f["value"])
	}
	return ""
}

// disclosure is the expand/collapse control for tool call i.
func disclosure(i int, open bool) string {
	glyph := "▸"
	if open {
		glyph = "▾"
	}
	return fmt.Sprintf(`<a class="disc" href="toggle:%d">%s</a>`, i, glyph)
}

// rawIO is the expanded detail: the arguments the tool was called with and the
// result it returned, as the model saw them. secret, when set, is redacted
// from both so a hidden finding's value stays masked here too.
func rawIO(t *agent.ToolCall, secret string) string {
	var b strings.Builder
	b.WriteString(`<pre class="io">`)
	if t.Input != "" {
		b.WriteString("args: " + html.EscapeString(maskSecret(t.Input, secret)) + "\n")
	}
	if t.Output != "" {
		b.WriteString(html.EscapeString(maskSecret(t.Output, secret)))
	}
	b.WriteString(`</pre>`)
	return b.String()
}

// maskSecret replaces every occurrence of secret in s with a mask, both as it
// appears raw and as it appears JSON-encoded (which is how a value sits inside
// a tool's arguments and result), so neither form leaks.
func maskSecret(s, secret string) string {
	if secret == "" {
		return s
	}
	const mask = "••••••"
	s = strings.ReplaceAll(s, secret, mask)
	if b, err := json.Marshal(secret); err == nil && len(b) >= 2 {
		if inner := string(b[1 : len(b)-1]); inner != secret {
			s = strings.ReplaceAll(s, inner, mask)
		}
	}
	return s
}

// toolCard renders a card for a tool call that created an object, or reports
// that this call isn't one. It reads what it can from the (possibly truncated)
// result, and always degrades to a card with just the icon and kind when the
// result can't be parsed.
func (p *agentsPanel) toolCard(i int, t *agent.ToolCall, revealed bool) (string, bool) {
	out := parseToolOutput(t.Output)
	switch {
	case t.Name == "create_finding" || t.Name == "set_finding_confidence" || t.Name == "set_finding_concern":
		kind := "Finding recorded"
		switch t.Name {
		case "set_finding_confidence":
			kind = "Finding scored"
		case "set_finding_concern":
			kind = "Finding assessed"
		}
		head, chips := "", ""
		if f := jmap(out["finding"]); f != nil {
			head = p.findingHeadline(i, jstr(f["value"]), jstr(f["notes"]), jbool(f["hidden"]), revealed)
			for _, id := range jstrs(f["types"]) {
				chips += p.typeChip(id)
			}
			chips += confidenceChip(f["confidence"]) + concernChip(f["concern"])
		}
		return cardHTML("finding", kind, head, "", chips, ""), true

	case t.ReplaySession != "":
		head := ""
		if tpl := jmap(out["template"]); tpl != nil {
			head = methodURL(tpl) // already HTML
		}
		action := fmt.Sprintf(`<a href="replay:%s">Open in Replay →</a>`, html.EscapeString(t.ReplaySession))
		return cardHTML("replay", "Replay session created", head, "", "", action), true

	case t.Name == "add_url":
		head := ""
		if e := jmap(out["entity"]); e != nil {
			head = html.EscapeString(elideRunes(jstr(e["key"]), 120))
		}
		return cardHTML("entity", "URL added to the Explorer", head, "", "", ""), true

	case t.Name == "set_entity_kv":
		head := ""
		if e := jmap(out["entity"]); e != nil {
			head = html.EscapeString(elideRunes(jstr(e["key"]), 120))
		}
		return cardHTML("entity", "Entity updated", head, "", "", ""), true

	case t.Name == "export_source_map":
		meta := ""
		if n, ok := out["files"].(float64); ok {
			meta = fmt.Sprintf("%d files recovered", int(n))
		}
		return cardHTML("sourcemap", "Sources recovered", html.EscapeString(elideRunes(jstr(out["dir"]), 120)), meta, "", ""), true
	}
	return "", false
}

// cardHTML lays out an object card: a square icon tile beside a surface panel
// holding the kind, headline, an optional meta line, chips and an action. Both
// cells share the surface background so the card reads as one panel; the tile's
// colour is baked into its image (see writeAgentIcons), so the cell is a plain
// square and doesn't stretch to the card's height.
//
// headline is trusted HTML (so a card can colour a method or add the reveal
// eye); callers escape plain text themselves.
func cardHTML(icon, kind, headline, meta, chips, action string) string {
	var body strings.Builder
	body.WriteString(fmt.Sprintf(`<span class="kind">%s</span>`, html.EscapeString(kind)))
	if headline != "" {
		body.WriteString(fmt.Sprintf(`<br><span class="headline">%s</span>`, headline))
	}
	if meta != "" {
		body.WriteString(fmt.Sprintf(`<br><span class="meta">%s</span>`, html.EscapeString(meta)))
	}
	if chips != "" {
		body.WriteString(`<br>` + chips)
	}
	if action != "" {
		body.WriteString(`<br>` + action)
	}
	return fmt.Sprintf(
		`<table width="100%%" border="0" cellspacing="0" cellpadding="6" style="margin:4px 0 9px 0;">`+
			`<tr><td width="40" bgcolor="%[1]s" align="center" valign="top"><img src="%[2]s.png" width="28" height="28"></td>`+
			`<td bgcolor="%[1]s" valign="top">%[3]s</td></tr></table>`,
		hex("surface"), icon, body.String())
}

// findingHeadline is the headline HTML for a finding card: the value, or — for
// a finding marked hidden (a secret in itself) — the value masked with an eye
// to reveal it, so it isn't sitting on screen for a passer-by. A hidden
// finding always has a value; one with only notes shows those plainly.
func (p *agentsPanel) findingHeadline(i int, value, notes string, hidden, revealed bool) string {
	if !hidden || value == "" {
		head := value
		if head == "" {
			head = notes
		}
		return html.EscapeString(elideRunes(head, 120))
	}
	if revealed {
		return html.EscapeString(elideRunes(value, 120)) + eyeControl(i, true)
	}
	return html.EscapeString(maskedValue(value)) + eyeControl(i, false)
}

// eyeControl is the reveal/hide eye beside a hidden finding's value.
func eyeControl(i int, revealed bool) string {
	img := "eye"
	if revealed {
		img = "eyeoff"
	}
	return fmt.Sprintf(` <a href="reveal:%d"><img src="%s.png" width="15" height="15"></a>`, i, img)
}

// maskedValue is how a hidden value reads while concealed: a few bullets and
// its length, which is often all a tester needs to tell two apart. It mirrors
// findings.Finding.Masked so the transcript and the Findings views agree.
func maskedValue(v string) string {
	n := len([]rune(v))
	if n == 0 {
		return ""
	}
	dots := n
	if dots > 8 {
		dots = 8
	}
	return strings.Repeat("•", dots) + fmt.Sprintf(" (%d characters)", n)
}

// typeChip is a finding type as a coloured pill, resolved against the user's
// catalog so it reads the same as in the Findings list; an unknown id shows as
// a plain chip.
func (p *agentsPanel) typeChip(id string) string {
	if t, ok := p.ctrl.Findings().Type(id); ok {
		return fmt.Sprintf(`<span class="chip" style="background-color:%s;color:#ffffff;">&nbsp;%s&nbsp;</span> `,
			t.Color, html.EscapeString(t.Name))
	}
	return fmt.Sprintf(`<span class="chip">&nbsp;%s&nbsp;</span> `, html.EscapeString(id))
}

// confidenceChip shows a finding's confidence (see confidence.go), coloured by
// band, or "not reviewed" when it has no score yet.
func confidenceChip(v any) string {
	if v == nil {
		return `<span class="chip">&nbsp;not reviewed&nbsp;</span>`
	}
	n, ok := v.(float64)
	if !ok {
		return ""
	}
	c := int(n)
	return fmt.Sprintf(`<span class="chip" style="color:%s;">&nbsp;%s&nbsp;</span>`,
		confidenceColor(c), html.EscapeString(confidenceText(&c)))
}

// concernChip shows a finding's concern (see confidence.go), coloured by band,
// or nothing when it hasn't been assessed yet (its absence is not noteworthy
// on a card the way an unreviewed confidence is).
func concernChip(v any) string {
	n, ok := v.(float64)
	if !ok {
		return ""
	}
	c := int(n)
	return fmt.Sprintf(` <span class="chip" style="color:%s;">&nbsp;concern: %s&nbsp;</span>`,
		concernColor(c), html.EscapeString(concernText(&c)))
}

// confidenceColor is the band colour for a score: real leans success, noise
// serverError, the floor between them held, and the exact midpoint muted.
func confidenceColor(c int) string {
	switch {
	case c > findings.ConfidenceUnknown:
		return hex(colorSuccess)
	case c == findings.ConfidenceUnknown:
		return hex(colorMuted)
	case c < findings.ConfidenceFloor:
		return hex(colorServerError)
	default:
		return hex("held")
	}
}

// readSummary is the one-line gist of a read/inspect tool's result, kept short
// since the full result is one click away.
func readSummary(t *agent.ToolCall) string {
	out := parseToolOutput(t.Output)
	if out == nil {
		return ""
	}
	switch t.Name {
	case "list_flows", "search_flows":
		if n, ok := out["matched"].(float64); ok {
			return fmt.Sprintf("%d requests", int(n))
		}
	case "list_findings":
		if n, ok := out["matched"].(float64); ok {
			return fmt.Sprintf("%d findings", int(n))
		}
	case "list_entities":
		if es, ok := out["entities"].([]any); ok {
			return fmt.Sprintf("%d entities", len(es))
		}
	case "get_flow":
		if s := jmap(out["summary"]); s != nil {
			return strings.TrimSpace(methodURL(s))
		}
	}
	return ""
}

// methodURL joins a request's method and URL from a parsed object, colouring
// the method the way the request list does.
func methodURL(m map[string]any) string {
	method, url := jstr(m["method"]), jstr(m["url"])
	if url == "" {
		url = jstr(m["path"])
	}
	if method == "" {
		return html.EscapeString(url)
	}
	return fmt.Sprintf(`<span style="color:%s;font-weight:bold;">%s</span> %s`,
		hex(methodColor(strings.ToUpper(method))), html.EscapeString(method), html.EscapeString(url))
}

func (p *agentsPanel) onAnchor(href string) {
	if h, ok := strings.CutPrefix(href, "replay:"); ok {
		if p.openRep != nil {
			p.openRep(h)
		}
		return
	}
	if h, ok := strings.CutPrefix(href, "toggle:"); ok {
		if i, err := strconv.Atoi(h); err == nil {
			p.expanded[i] = !p.expanded[i]
			p.rerender()
		}
		return
	}
	if h, ok := strings.CutPrefix(href, "reveal:"); ok {
		if i, err := strconv.Atoi(h); err == nil {
			p.revealed[i] = !p.revealed[i]
			p.rerender()
		}
	}
}

// rerender rebuilds the whole transcript (after a toggle or a theme change),
// keeping the scroll position roughly where it was.
func (p *agentsPanel) rerender() {
	s, ok := p.mgr.Get(p.current)
	if !ok {
		return
	}
	sb := p.transcript.VerticalScrollBar()
	pos := sb.Value()
	p.transcript.SetHtml(p.renderTranscript(s))
	p.rendered = len(s.Messages)
	sb.SetValue(pos)
}

func (p *agentsPanel) scrollToEnd() {
	sb := p.transcript.VerticalScrollBar()
	sb.SetValue(sb.Maximum())
}

// ---- helpers ---------------------------------------------------------------

var (
	reBold = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reCode = regexp.MustCompile("`([^`]+)`")
)

// mdInline renders the light markdown the model tends to write — **bold** and
// `code` — after escaping, and turns newlines into breaks. It is deliberately
// small: escaping first means the markers can only wrap already-safe text.
func mdInline(s string) string {
	s = html.EscapeString(s)
	s = reBold.ReplaceAllString(s, "<b>$1</b>")
	s = reCode.ReplaceAllString(s, "<code>$1</code>")
	return strings.ReplaceAll(s, "\n", "<br>")
}

// parseToolOutput best-effort decodes a tool result as a JSON object. A
// truncated result (the runtime caps what the model sees) won't parse; the
// caller then falls back to a bare card or no summary.
func parseToolOutput(out string) map[string]any {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(out), &m) != nil {
		return nil
	}
	return m
}

func jstr(v any) string { s, _ := v.(string); return s }

func jbool(v any) bool { b, _ := v.(bool); return b }

func jmap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func jstrs(v any) []string {
	a, _ := v.([]any)
	out := make([]string, 0, len(a))
	for _, e := range a {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// elideRunes shortens a headline to n runes with an ellipsis, counting runes
// so a cut never splits one.
func elideRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
