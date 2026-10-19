package cli

import (
	"fmt"
	"sort"
	"strings"
)

// Help is written here rather than by the client, so what a user is told
// always comes from the process that would run the command: a plugin's
// namespace is in the list the moment the plugin is enabled.

const usageLine = "Usage: reproxy <namespace> <action> [--flags] [arguments]"

// overview is `reproxy help`: the namespaces, and how to go deeper.
func (r *Registry) overview() string {
	var b strings.Builder
	b.WriteString("reproxy — intercept, edit and replay HTTP traffic\n\n")
	b.WriteString(usageLine + "\n")
	b.WriteString("       reproxy                             start the app\n")
	b.WriteString("       reproxy shell                       run commands interactively\n\n")

	all := r.All()
	core, plugins := map[string][]Command{}, map[string][]Command{}
	for _, c := range all {
		if c.Plugin == "" {
			core[c.Namespace] = append(core[c.Namespace], c)
		} else {
			plugins[c.Namespace] = append(plugins[c.Namespace], c)
		}
	}

	b.WriteString("Core namespaces:\n")
	var rows [][2]string
	for _, ns := range CoreNamespaces() {
		if len(core[ns.Name]) == 0 {
			continue
		}
		rows = append(rows, [2]string{ns.Name, ns.Summary})
	}
	b.WriteString(table(rows, "  "))

	if len(plugins) > 0 {
		b.WriteString("\nPlugin namespaces:\n")
		rows = rows[:0]
		names := make([]string, 0, len(plugins))
		for ns := range plugins {
			names = append(names, ns)
		}
		sort.Strings(names)
		for _, ns := range names {
			rows = append(rows, [2]string{ns, "from the " + plugins[ns][0].Plugin + " plugin"})
		}
		b.WriteString(table(rows, "  "))
	}

	if waiting := r.unimplemented(core); len(waiting) > 0 {
		b.WriteString("\nReserved for core, with no commands yet:\n  " + wrap(strings.Join(waiting, ", "), 72, "  ") + "\n")
	}

	b.WriteString("\nRun \"reproxy <namespace>\" to see its actions, or\n")
	b.WriteString("\"reproxy <namespace> <action> --help\" for one command's flags.\n")
	b.WriteString("\nGlobal flags:\n")
	b.WriteString(table([][2]string{
		{"--instance <id>", "the reproxy process to run in (default: the hub's own)"},
		{"--json", "answer as JSON"},
		{"--help", "usage for the command"},
	}, "  "))
	return b.String()
}

// unimplemented are the reserved core namespaces nothing is registered in
// yet. Saying so is the honest thing while core is being built out: the
// name is taken, but nothing answers to it.
func (r *Registry) unimplemented(have map[string][]Command) []string {
	var out []string
	for _, ns := range CoreNamespaces() {
		if len(have[ns.Name]) == 0 {
			out = append(out, ns.Name)
		}
	}
	return out
}

// namespaceHelp is `reproxy <namespace>`: what it acts on and its actions.
func (r *Registry) namespaceHelp(ns string, cmds []Command) string {
	var b strings.Builder
	if core, ok := Lookup(ns); ok {
		fmt.Fprintf(&b, "reproxy %s — %s\n", ns, core.Summary)
	} else {
		fmt.Fprintf(&b, "reproxy %s — from the %s plugin\n", ns, cmds[0].Plugin)
	}
	b.WriteString("\nActions:\n")
	rows := make([][2]string, 0, len(cmds))
	for _, c := range cmds {
		rows = append(rows, [2]string{c.Action, c.Summary})
	}
	b.WriteString(table(rows, "  "))
	fmt.Fprintf(&b, "\nRun \"reproxy %s <action> --help\" for one command's flags.\n", ns)
	return b.String()
}

// commandHelp is `reproxy <namespace> <action> --help`.
func commandHelp(c Command) string {
	var b strings.Builder
	fmt.Fprintf(&b, "reproxy %s %s", c.Namespace, c.Action)
	if c.Summary != "" {
		fmt.Fprintf(&b, " — %s", c.Summary)
	}
	b.WriteString("\n")
	if c.Plugin != "" {
		fmt.Fprintf(&b, "(from the %s plugin)\n", c.Plugin)
	}
	if c.Description != "" {
		b.WriteString("\n" + wrap(c.Description, 72, "") + "\n")
	}

	var usage strings.Builder
	fmt.Fprintf(&usage, "reproxy %s %s", c.Namespace, c.Action)
	for _, f := range c.Flags {
		if !f.Required {
			continue
		}
		fmt.Fprintf(&usage, " --%s %s", f.Name, placeholder(f))
	}
	if len(c.Flags) > len(required(c.Flags)) {
		usage.WriteString(" [--flags]")
	}
	if c.Args != "" {
		usage.WriteString(" " + c.Args)
	}
	b.WriteString("\nUsage:\n  " + usage.String() + "\n")

	if len(c.Flags) > 0 {
		b.WriteString("\nFlags:\n")
		rows := make([][2]string, 0, len(c.Flags))
		for _, f := range c.Flags {
			left := "--" + f.Name
			if f.Short != "" {
				left = "-" + f.Short + ", " + left
			}
			if f.Type != FlagBool {
				left += " " + placeholder(f)
			}
			right := f.Description
			switch {
			case f.Required:
				right = strings.TrimSpace(right + " (required)")
			case f.Default != "":
				right = strings.TrimSpace(right + fmt.Sprintf(" (default %s)", f.Default))
			}
			rows = append(rows, [2]string{left, right})
		}
		b.WriteString(table(rows, "  "))
	}
	return b.String()
}

func required(flags []Flag) []Flag {
	var out []Flag
	for _, f := range flags {
		if f.Required {
			out = append(out, f)
		}
	}
	return out
}

func placeholder(f Flag) string {
	if f.Placeholder != "" {
		return "<" + strings.Trim(f.Placeholder, "<>") + ">"
	}
	switch f.Type {
	case FlagNumber:
		return "<number>"
	case FlagList:
		return "<a,b,c>"
	case FlagBool:
		return ""
	}
	return "<value>"
}

// StaticHelp is what the client prints when no reproxy process is running:
// the reserved core namespaces, from the table alone. It can't know a
// plugin's commands, and says so.
func StaticHelp() string {
	var b strings.Builder
	b.WriteString("reproxy — intercept, edit and replay HTTP traffic\n\n" + usageLine + "\n\n")
	b.WriteString("No reproxy process is running, so this is the reserved core namespace\nlist only — start reproxy to see the commands it and its plugins offer.\n\n")
	rows := make([][2]string, 0, len(coreNamespaces))
	for _, ns := range CoreNamespaces() {
		rows = append(rows, [2]string{ns.Name, ns.Summary})
	}
	b.WriteString(table(rows, "  "))
	return b.String()
}

// table lays out two columns, the first padded to the widest entry.
func table(rows [][2]string, indent string) string {
	width := 0
	for _, r := range rows {
		width = max(width, len(r[0]))
	}
	var b strings.Builder
	for _, r := range rows {
		if r[1] == "" {
			b.WriteString(indent + r[0] + "\n")
			continue
		}
		fmt.Fprintf(&b, "%s%-*s  %s\n", indent, width, r[0], r[1])
	}
	return b.String()
}

// wrap breaks text at width, indenting every line after the first.
func wrap(s string, width int, indent string) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > width {
			b.WriteString(line + "\n" + indent)
			line = w
			continue
		}
		line += " " + w
	}
	b.WriteString(line)
	return b.String()
}

// Table lays out a listing in aligned columns: a header row, then the rows
// themselves. It is what the list commands print, so `reproxy plugins ls`
// and `reproxy instances ls` line up the same way.
func Table(header []string, rows [][]string) string {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len([]rune(h))
	}
	for _, r := range rows {
		for i := range r {
			if i < len(widths) {
				widths[i] = max(widths[i], len([]rune(r[i])))
			}
		}
	}
	var b strings.Builder
	write := func(cells []string) {
		line := ""
		for i, c := range cells {
			if i == len(cells)-1 || i >= len(widths) {
				line += c
				continue
			}
			// Padded by runes, not bytes: a column holding a host name or
			// a plugin's own words can be any text at all.
			line += c + strings.Repeat(" ", widths[i]-len([]rune(c))+2)
		}
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	write(header)
	for _, r := range rows {
		write(r)
	}
	return b.String()
}

// Help is one command's usage: what `--help` prints. The command line
// prints it for the commands it runs itself; everything else is answered by
// the instance.
func Help(c Command) string { return commandHelp(c) }
