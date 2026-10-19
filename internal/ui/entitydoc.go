package ui

import (
	"fmt"
	"html"
	"strconv"
	"strings"

	"reproxy/internal/capture"
	"reproxy/internal/elide"
	"reproxy/internal/entities"
	"reproxy/internal/sitemap"
	"reproxy/internal/target"
)

// entityDocHTML renders a node of the Explorer for its sidebar: what all
// entities show (their type, whether traffic reached them, their target,
// their key/value store), and then what their type has — a domain's public
// suffix and hosts; a host's port and servers, or the addresses it
// resolves to; and a URL's Host section (see writeHostSection), then the
// URL laid out like an OpenAPI (Swagger) path item: a coloured block per
// method, then its parameters, request body and responses. Qt's rich text supports tables
// and colours but little CSS, so the layout is table-based and the theme's
// colours are written in directly (re-render on a theme change).
func entityDocHTML(n *sitemap.Node, targets *target.Store, hosts hostDetails) string {
	var b strings.Builder
	text, muted, border := hex(colorText), hex(colorMuted), hex("border")
	fmt.Fprintf(&b, `<div style="color:%s">`, text)

	tc := hex(typeColor(n.Type))
	kind := strings.TrimSuffix(typeLabels[n.Type], "s")
	if n.Entity == nil {
		kind = "Path"
	}
	fmt.Fprintf(&b, `<p style="margin:0 0 10px 0"><span style="background-color:%s; color:#ffffff; font-weight:600">&nbsp;%s&nbsp;</span>&nbsp; <span style="font-family:monospace; color:%s">%s</span></p>`,
		tc, esc(kind), muted, esc(n.Label()))

	e := n.Entity
	switch {
	case e == nil:
		fmt.Fprintf(&b, `<p>Nothing was requested at exactly this path. %s below it.</p>`, countOf(n.Total, "request", "requests"))
		writeChildren(&b, n)
	case !e.Visited() && e.Origin == entities.OriginTarget:
		b.WriteString(`<p><b>Named by a target</b>: no traffic has reached it yet.</p>`)
	case !e.Visited():
		b.WriteString(`<p><b>Discovered</b>: referred to, but no traffic has reached it yet.</p>`)
	case n.IsURL():
		fmt.Fprintf(&b, `<p><b>Visited</b>: %s.</p>`, countOf(e.Hits, "request", "requests"))
	default:
		fmt.Fprintf(&b, `<p><b>Visited</b>: %s to the URLs under it.</p>`, countOf(e.Hits, "request", "requests"))
	}
	if e == nil {
		b.WriteString(`</div>`)
		return b.String()
	}

	var facts [][]string
	if t, ok := targets.Get(n.TargetID); ok {
		facts = append(facts, []string{"Target", fmt.Sprintf(`<span style="color:%s">■</span> %s`, t.Color, esc(t.Label()))})
	}
	facts = append(facts, []string{"Found by", esc(originText(e.Origin))})
	switch {
	case e.DomainAttrs != nil:
		suffix := e.DomainAttrs.Suffix
		if !e.DomainAttrs.ICANN {
			suffix += " (private)"
		}
		facts = append(facts, []string{"Public suffix", monoCell(suffix)},
			[]string{"Hosts", strconv.Itoa(len(n.Children))})
	case e.HostAttrs != nil:
		a := e.HostAttrs
		if e.Scheme != "" {
			facts = append(facts, []string{"Scheme", monoCell(e.Scheme)}, []string{"Port", strconv.Itoa(a.Port)})
		} else {
			facts = append(facts, []string{"Origins", strconv.Itoa(len(n.Children))})
		}
		facts = append(facts, hostFacts(e, hosts.resolvedFrom)...)
	case e.URLAttrs != nil:
		var kinds []string
		for _, k := range e.URLAttrs.Kinds {
			kinds = append(kinds, kindLabels[k])
		}
		if len(kinds) > 0 {
			facts = append(facts, []string{"Content", esc(strings.Join(kinds, ", "))})
		}
	}
	if !e.FirstSeen.IsZero() {
		facts = append(facts, []string{"First seen", esc(e.FirstSeen.Format("2006-01-02 15:04:05"))})
	}
	writeFacts(&b, facts)

	writeHeading(&b, "Details")
	if len(e.KV) == 0 {
		fmt.Fprintf(&b, `<p style="color:%s">Nothing kept on it yet. Edit Details adds name and value pairs.</p>`, muted)
	} else {
		var rows [][]string
		for _, p := range e.KV {
			rows = append(rows, []string{monoCell(p.Key), esc(p.Value)})
		}
		writeTable(&b, []string{"Name", "Value"}, rows)
	}

	if e.URLAttrs != nil && hosts.origin != nil {
		writeHostSection(&b, hosts)
	}

	if u := e.URLAttrs; u != nil {
		for _, op := range u.Operations {
			writeOperation(&b, n, e, op, border, muted)
		}
		if len(u.Examples) > 0 {
			writeHeading(&b, "Paths seen")
			writeList(&b, u.Examples, true)
		}
	} else {
		writeChildren(&b, n)
	}
	if linked := e.LinkedFrom(); len(linked) > 0 {
		writeHeading(&b, "Linked from")
		writeList(&b, linked, true)
	}
	b.WriteString(`</div>`)
	return b.String()
}

// hostDetails are the hosts a node's sidebar describes besides the node
// itself, looked up in the store (an address out of scope isn't in the
// tree): for a URL, its origin, the name host above it and the hosts of
// the addresses that name resolves to; for an IP address, the names that
// resolve to it.
type hostDetails struct {
	// open says whether a URL's Host section is expanded.
	open         bool
	origin, name *entities.Entity
	addresses    []entities.Entity
	resolvedFrom []string
}

// hostSectionLink is the anchor that opens and closes a URL's Host
// section.
const hostSectionLink = "explorer:host"

// hostFacts are what a host's facts table shows about what it is: an IP
// address, its servers, the addresses it resolves to, the names resolving
// to it.
func hostFacts(e *entities.Entity, resolvedFrom []string) [][]string {
	a := e.HostAttrs
	var facts [][]string
	if a.IP {
		facts = append(facts, []string{"Address", "IP address"})
	}
	if len(a.Servers) > 0 {
		facts = append(facts, []string{"Server", monoCell(strings.Join(a.Servers, ", "))})
	}
	if len(a.Addresses) > 0 {
		facts = append(facts, []string{"Resolves to", monoCell(strings.Join(a.Addresses, ", "))})
	}
	if len(resolvedFrom) > 0 {
		facts = append(facts, []string{"Resolved from", monoCell(strings.Join(resolvedFrom, ", "))})
	}
	return facts
}

// writeHostSection writes a URL's Host section: a header that opens and
// closes it (closed until the user opens it), and when open what is known
// about the origin the URL is on, its name, and the addresses that name
// resolves to — each one's facts and key/value store.
func writeHostSection(b *strings.Builder, h hostDetails) {
	muted, border := hex(colorMuted), hex("border")
	arrow := "▸"
	if h.open {
		arrow = "▾"
	}
	summary := h.origin.Label()
	if h.name != nil && len(h.name.HostAttrs.Addresses) > 0 {
		summary += " · " + countOf(len(h.name.HostAttrs.Addresses), "address", "addresses")
	}
	fmt.Fprintf(b, `<table width="100%%" cellspacing="0" cellpadding="6" style="margin-top:14px; border:1px solid %s; background-color:%s"><tr><td>`,
		border, mixHex(hex(typeColor(sitemap.TypeHost)), hex("surface"), 0.08))
	fmt.Fprintf(b, `<a href="%s" style="text-decoration:none; color:%s"><b>%s&nbsp; Host</b></a>&nbsp; <span style="color:%s; font-family:monospace">%s</span>`,
		hostSectionLink, hex(colorText), arrow, muted, esc(summary))
	b.WriteString(`</td></tr></table>`)
	if !h.open {
		return
	}
	writeHostPart(b, "Origin", h.origin)
	if h.name != nil {
		writeHostPart(b, "Name", h.name)
	}
	for i := range h.addresses {
		writeHostPart(b, "Address", &h.addresses[i])
	}
	if h.name != nil && len(h.name.HostAttrs.Addresses) == 0 {
		fmt.Fprintf(b, `<p style="color:%s">No addresses are known for %s yet: a plugin (such as dnsdumpster) can add them.</p>`, muted, esc(h.name.Label()))
	}
}

// writeHostPart describes one host in a URL's Host section.
func writeHostPart(b *strings.Builder, role string, e *entities.Entity) {
	muted := hex(colorMuted)
	fmt.Fprintf(b, `<p style="margin:12px 0 4px 0"><b>%s</b>&nbsp; <span style="font-family:monospace">%s</span>`, esc(role), esc(e.Label()))
	if !e.Visited() {
		fmt.Fprintf(b, `&nbsp; <i style="color:%s">discovered</i>`, muted)
	}
	b.WriteString(`</p>`)
	var facts [][]string
	if e.Scheme != "" {
		facts = append(facts, []string{"Port", strconv.Itoa(e.HostAttrs.Port)})
	}
	if e.Hits > 0 {
		facts = append(facts, []string{"Requests", strconv.Itoa(e.Hits)})
	}
	for _, f := range hostFacts(e, nil) {
		if f[0] != "Address" { // the heading says so
			facts = append(facts, f)
		}
	}
	facts = append(facts, []string{"Found by", esc(originText(e.Origin))})
	for _, p := range e.KV {
		facts = append(facts, []string{p.Key, esc(p.Value)})
	}
	writeFacts(b, facts)
}

// originText says what first put an entity in the Explorer.
func originText(origin string) string {
	switch origin {
	case entities.OriginTraffic:
		return "Traffic"
	case entities.OriginLink:
		return "A link in a response"
	case entities.OriginReferer:
		return "A request's Referer or Origin"
	case entities.OriginTarget:
		return "A target"
	case "agent":
		return "An agent"
	}
	if name, ok := strings.CutPrefix(origin, "plugin:"); ok {
		return "The plugin " + name
	}
	return origin
}

// writeFacts writes label/value rows whose values are already HTML.
func writeFacts(b *strings.Builder, rows [][]string) {
	muted := hex(colorMuted)
	b.WriteString(`<table cellspacing="0" cellpadding="3">`)
	for _, r := range rows {
		fmt.Fprintf(b, `<tr><td style="color:%s; padding-right:14px">%s</td><td>%s</td></tr>`, muted, esc(r[0]), r[1])
	}
	b.WriteString(`</table>`)
}

func writeOperation(b *strings.Builder, n *sitemap.Node, ep *sitemap.Entity, op *sitemap.Operation, border, muted string) {
	mc := hex(methodColor(op.Method))
	fmt.Fprintf(b, `<table width="100%%" cellspacing="0" cellpadding="7" style="margin-top:14px; border:1px solid %s; background-color:%s"><tr><td>`,
		mixHex(mc, hex("surface"), 0.45), mixHex(mc, hex("surface"), 0.1))
	fmt.Fprintf(b, `<span style="background-color:%s; color:#ffffff; font-weight:600">&nbsp;%s&nbsp;</span>&nbsp; `, mc, esc(op.Method))
	fmt.Fprintf(b, `<span style="font-family:monospace; font-weight:600">%s</span>`, esc(n.Path))
	fmt.Fprintf(b, `<span style="color:%s">&nbsp; · %s</span>`, muted, countOf(op.Requests, "request", "requests"))
	b.WriteString(`</td></tr></table>`)

	// Parameters: the path's templated segments, then the query string.
	var rows [][]string
	for _, p := range ep.URLAttrs.PathParams {
		rows = append(rows, paramRow(p, "path"))
	}
	for _, p := range op.Query {
		rows = append(rows, paramRow(p, "query"))
	}
	writeHeading(b, "Parameters")
	if len(rows) == 0 {
		fmt.Fprintf(b, `<p style="color:%s">No parameters</p>`, muted)
	} else {
		writeTable(b, []string{"Name", "In", "Type", "Example"}, rows)
	}

	if len(op.BodyTypes) > 0 || len(op.Body) > 0 {
		writeHeading(b, "Request body")
		if len(op.BodyTypes) > 0 {
			fmt.Fprintf(b, `<p style="font-family:monospace; color:%s; margin:0 0 4px 0">%s</p>`, muted, esc(strings.Join(op.BodyTypes, ", ")))
		}
		var body [][]string
		for _, p := range op.Body {
			r := paramRow(p, "")
			body = append(body, []string{r[0], r[2], r[3]})
		}
		if len(body) > 0 {
			writeTable(b, []string{"Name", "Type", "Example"}, body)
		}
	}

	writeHeading(b, "Responses")
	if len(op.Responses) == 0 {
		fmt.Fprintf(b, `<p style="color:%s">No response recorded</p>`, muted)
		return
	}
	var resp [][]string
	for _, r := range op.Responses {
		code := fmt.Sprintf(`<span style="color:%s; font-weight:600">%d</span>`, hex(responseColor(r.Status)), r.Status)
		desc := strings.TrimSpace(strings.TrimPrefix(r.Text, strconv.Itoa(r.Status)))
		resp = append(resp, []string{code, esc(desc), monoCell(strings.Join(r.ContentTypes, ", ")), strconv.Itoa(r.Count)})
	}
	writeTable(b, []string{"Code", "Description", "Content type", "Seen"}, resp)
}

// paramRow is a parameter's cells: name (starred when required), where it
// goes, its type and an example value. Cells are HTML.
func paramRow(p *sitemap.Param, in string) []string {
	name := monoCell(p.Name)
	if p.Required {
		name += fmt.Sprintf(`<span style="color:%s">&nbsp;*</span>`, hex(colorServerError))
	}
	example := ""
	if len(p.Examples) > 0 {
		example = monoCell(elide.Runes(p.Examples[0], 40))
	}
	return []string{name, esc(in), fmt.Sprintf(`<i>%s</i>`, esc(p.Type)), example}
}

func writeHeading(b *strings.Builder, title string) {
	fmt.Fprintf(b, `<p style="font-weight:600; margin:12px 0 4px 0">%s</p>`, esc(title))
}

// writeTable writes a table whose cells are already HTML.
func writeTable(b *strings.Builder, head []string, rows [][]string) {
	muted, rule := hex(colorMuted), hex("borderSubtle")
	b.WriteString(`<table width="100%" cellspacing="0" cellpadding="4">`)
	b.WriteString(`<tr>`)
	for _, h := range head {
		fmt.Fprintf(b, `<td style="color:%s; border-bottom:1px solid %s"><small>%s</small></td>`, muted, rule, esc(strings.ToUpper(h)))
	}
	b.WriteString(`</tr>`)
	for _, r := range rows {
		b.WriteString(`<tr>`)
		for _, c := range r {
			fmt.Fprintf(b, `<td style="border-bottom:1px solid %s">%s</td>`, rule, c)
		}
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</table>`)
}

func writeList(b *strings.Builder, items []string, mono bool) {
	b.WriteString(`<ul style="margin:0">`)
	for _, it := range items {
		if mono {
			fmt.Fprintf(b, `<li>%s</li>`, monoCell(it))
		} else {
			fmt.Fprintf(b, `<li>%s</li>`, esc(it))
		}
	}
	b.WriteString(`</ul>`)
}

// writeChildren lists what sits directly below a node: a domain's hosts,
// a host's paths, the paths below a path prefix.
func writeChildren(b *strings.Builder, n *sitemap.Node) {
	if len(n.Children) == 0 {
		return
	}
	title := "Below this path"
	switch {
	case n.Type == sitemap.TypeDomain:
		title = "Hosts"
	case n.Type == sitemap.TypeHost && n.Scheme == "":
		title = "Origins"
	case n.Type == sitemap.TypeHost:
		title = "Paths"
	}
	writeHeading(b, title)
	var items []string
	for _, c := range n.Children {
		if c.IsURL() {
			items = append(items, c.Path)
		} else {
			items = append(items, c.Label())
		}
	}
	writeList(b, items, true)
}

func monoCell(s string) string {
	return `<span style="font-family:monospace">` + esc(s) + `</span>`
}

func esc(s string) string { return html.EscapeString(s) }

// countOf is n with its noun, e.g. "1 request", "3 requests".
func countOf(n int, one, many string) string {
	return strconv.Itoa(n) + " " + plural(n, one, many)
}

// responseColor is the theme colour for a status code, as in the request
// list.
func responseColor(code int) string {
	_, c := statusLabel(capture.Flow{State: capture.StateComplete, StatusCode: code})
	return c
}

// mixHex blends two "#rrggbb" colours: t of a, the rest b.
func mixHex(a, b string, t float64) string {
	pa, pb := parseHex(a), parseHex(b)
	var out [3]int
	for i := range out {
		out[i] = int(float64(pa[i])*t + float64(pb[i])*(1-t) + 0.5)
	}
	return fmt.Sprintf("#%02x%02x%02x", out[0], out[1], out[2])
}

func parseHex(s string) [3]int {
	var c [3]int
	if len(s) == 7 && s[0] == '#' {
		for i := range c {
			v, _ := strconv.ParseUint(s[1+2*i:3+2*i], 16, 8)
			c[i] = int(v)
		}
	}
	return c
}
