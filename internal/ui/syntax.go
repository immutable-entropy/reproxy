package ui

import (
	"strings"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/syntaxcheck"
)

// Language identifies how the body editor highlights a message body.
type Language string

const (
	LangPlain Language = "Plain text"
	LangJSON  Language = "JSON"
	LangHTML  Language = "HTML"
	LangXML   Language = "XML"
	LangJS    Language = "JavaScript"
	LangCSS   Language = "CSS"
)

// allLanguages is the order shown in the language selector.
var allLanguages = []Language{LangPlain, LangJSON, LangHTML, LangXML, LangJS, LangCSS}

// detectLanguage guesses a body's language from its Content-Type, falling
// back to a sniff of the content itself.
func detectLanguage(contentType, body string) Language {
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "json"):
		return LangJSON
	case strings.Contains(ct, "html"):
		return LangHTML
	case strings.Contains(ct, "xml"):
		return LangXML
	case strings.Contains(ct, "javascript"), strings.Contains(ct, "ecmascript"),
		// A recovered file is often TypeScript or a framework's single-file
		// component; the JavaScript highlighter is the right one for all of
		// them (see internal/files.MediaType).
		strings.Contains(ct, "typescript"), strings.Contains(ct, "jsx"),
		strings.Contains(ct, "tsx"), strings.Contains(ct, "x-vue"),
		strings.Contains(ct, "x-svelte"):
		return LangJS
	case strings.Contains(ct, "css"):
		return LangCSS
	case ct != "" && strings.HasPrefix(ct, "text/"):
		return LangPlain
	}
	return sniffLanguage(body)
}

func sniffLanguage(body string) Language {
	t := strings.TrimSpace(body)
	switch {
	case t == "":
		return LangPlain
	case strings.HasPrefix(t, "{"), strings.HasPrefix(t, "["):
		return LangJSON
	case strings.HasPrefix(strings.ToLower(t), "<!doctype html"), strings.HasPrefix(strings.ToLower(t), "<html"):
		return LangHTML
	case strings.HasPrefix(t, "<?xml"), strings.HasPrefix(t, "<"):
		return LangXML
	}
	return LangPlain
}

// checkerFor returns the syntax check for a language, or nil if there is none.
func checkerFor(lang Language) func(string) []syntaxcheck.Issue {
	switch lang {
	case LangJSON:
		return syntaxcheck.JSON
	case LangXML:
		return syntaxcheck.XML
	case LangHTML:
		return syntaxcheck.HTML
	case LangJS:
		return syntaxcheck.JavaScript
	case LangCSS:
		return syntaxcheck.CSS
	}
	return nil
}

// Highlight colours come from the active theme's palette (see theme.go), keyed
// by synKeyword, synString, … so switching themes recolours the syntax too.

// rule is one highlighting rule: every match of re is coloured, using capture
// group group (0 = whole match).
type rule struct {
	re    *qt.QRegularExpression
	color *qt.QColor
	group int
}

// blockComment describes a multi-line comment style handled with block state.
type blockComment struct {
	start, end string
	color      *qt.QColor
}

// highlighter is a reusable rule-based syntax highlighter whose language can
// be switched at runtime.
type highlighter struct {
	*qt.QSyntaxHighlighter
	rules   []rule
	comment *blockComment
}

func newHighlighter(doc *qt.QTextDocument) *highlighter {
	h := &highlighter{QSyntaxHighlighter: qt.NewQSyntaxHighlighter2(doc)}
	h.OnHighlightBlock(h.highlightBlock)
	return h
}

// setLanguage swaps the active rule set and re-highlights the document.
func (h *highlighter) setLanguage(lang Language) {
	h.rules, h.comment = rulesFor(lang)
	h.Rehighlight()
}

func (h *highlighter) highlightBlock(text string) {
	for _, r := range h.rules {
		it := r.re.GlobalMatch(text)
		for it.HasNext() {
			m := it.Next()
			start := m.CapturedStartWithNth(r.group)
			length := m.CapturedLengthWithNth(r.group)
			if start >= 0 && length > 0 {
				h.SetFormat2(int(start), int(length), r.color)
			}
		}
	}
	h.highlightBlockComments(text)
}

// highlightBlockComments colours multi-line comments using the block state to
// carry "inside a comment" across lines.
func (h *highlighter) highlightBlockComments(text string) {
	h.SetCurrentBlockState(0)
	if h.comment == nil {
		return
	}
	start := 0
	if h.PreviousBlockState() != 1 {
		start = strings.Index(text, h.comment.start)
	}
	for start >= 0 {
		end := strings.Index(text[start:], h.comment.end)
		var length int
		if end < 0 {
			h.SetCurrentBlockState(1)
			length = len(text) - start
		} else {
			length = end + len(h.comment.end)
		}
		h.SetFormat2(start, length, h.comment.color)
		rest := start + length
		if rest >= len(text) {
			break
		}
		next := strings.Index(text[rest:], h.comment.start)
		if next < 0 {
			break
		}
		start = rest + next
	}
}

func re(pattern string) *qt.QRegularExpression {
	return qt.NewQRegularExpression2(pattern)
}

func reCI(pattern string) *qt.QRegularExpression {
	return qt.NewQRegularExpression4(pattern, qt.QRegularExpression__CaseInsensitiveOption)
}

// rulesFor returns the highlighting rules and multi-line comment style for a
// language.
func rulesFor(lang Language) ([]rule, *blockComment) {
	c := func(key string) *qt.QColor { return color(key) }
	switch lang {
	case LangJSON:
		return []rule{
			{re(`"(?:\\.|[^"\\])*"\s*:`), c(synKey), 0},
			{re(`:\s*("(?:\\.|[^"\\])*")`), c(synString), 1},
			{re(`\b-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?\b`), c(synNumber), 0},
			{re(`\b(?:true|false|null)\b`), c(synLiteral), 0},
		}, nil
	case LangJS:
		return []rule{
			{re(`\b(?:var|let|const|function|return|if|else|for|while|do|switch|case|break|continue|new|typeof|instanceof|this|class|extends|super|import|export|from|default|async|await|yield|try|catch|finally|throw|delete|in|of|void)\b`), c(synKeyword), 0},
			{re(`\b(?:true|false|null|undefined|NaN|Infinity)\b`), c(synLiteral), 0},
			{re(`"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|` + "`(?:\\\\.|[^`\\\\])*`"), c(synString), 0},
			{re(`\b-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?\b`), c(synNumber), 0},
			{re(`//[^\n]*`), c(synComment), 0},
		}, &blockComment{"/*", "*/", c(synComment)}
	case LangCSS:
		return []rule{
			{re(`[.#]?[-\w]+(?=\s*\{)`), c(synTag), 0},
			{re(`[-\w]+(?=\s*:)`), c(synAttr), 0},
			{re(`:\s*([^;{}]+)`), c(synString), 1},
			{re(`#[0-9a-fA-F]{3,8}\b`), c(synNumber), 0},
		}, &blockComment{"/*", "*/", c(synComment)}
	case LangHTML, LangXML:
		return []rule{
			{reCI(`</?[a-z][\w:-]*`), c(synTag), 0},
			{re(`>`), c(synTag), 0},
			{reCI(`\b[a-z][\w:-]*(?==)`), c(synAttr), 0},
			{re(`"(?:\\.|[^"\\])*"|'(?:[^']*)'`), c(synString), 0},
		}, &blockComment{"<!--", "-->", c(synComment)}
	}
	return nil, nil
}
