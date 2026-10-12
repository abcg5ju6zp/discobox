package tui

import (
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"charm.land/glamour/v2/ansi"
	glamourstyles "charm.land/glamour/v2/styles"
)

// charRef is a character reference as the renderer reads one: `&#27;`,
// `&#x1b;` or `&rlm;` — and without the semicolon too, which it takes as a
// browser does (`&#x1b[31m` is an escape and `[31m`).
var charRef = regexp.MustCompile(`&(?:#[0-9]+|#[xX][0-9a-fA-F]+|[A-Za-z][A-Za-z0-9]*);?`)

// markdownSource is a comment's markdown as it is handed to the renderer. The
// data source has already escaped every character a terminal would act on
// (Issue), but the renderer decodes character references, so `&#x1b;[31m` is
// printable text there and an escape sequence once drawn — and `&#x202e;` a
// bidirectional override, `&#x200b;` a zero-width space. A reference to a
// character that would not be let through as itself is therefore left
// undecoded, by writing its `&` as `&amp;`: it is shown as it was typed, not
// obeyed. The rest — `&amp;`, `&lt;`, `&nbsp;`, `&#128512;` — decode as they
// do on the page.
//
// It is done to the whole body, code included, where the renderer decodes
// nothing and such a reference is drawn with the `amp;` in it. Sparing code
// would mean knowing where a fence is as the parser knows it — one inside a
// list item ends with the item, one inside an HTML block is not one — and a
// guess that said "code" where the parser says "prose" is the escape again.
func markdownSource(body string) string {
	shown := func(r rune) bool { return unicode.IsPrint(r) || unicode.Is(unicode.Zs, r) }
	return charRef.ReplaceAllStringFunc(body, func(ref string) string {
		typed := "&amp;" + ref[1:]
		if ref[1] == '#' {
			// The number as the character it names, as well as what html
			// makes of it below: it maps some (0x80–0x9f) as a browser would,
			// and not every reader of the markdown does.
			digits, base := strings.TrimSuffix(ref[2:], ";"), 10
			if digits[0] == 'x' || digits[0] == 'X' {
				digits, base = digits[1:], 16
			}
			if n, err := strconv.ParseUint(digits, base, 32); err != nil || n > unicode.MaxRune || !shown(rune(n)) {
				return typed
			}
		}
		// A name there is not decodes to itself, and is text already.
		for _, r := range html.UnescapeString(ref) {
			if !shown(r) {
				return typed
			}
		}
		return ref
	})
}

// markdownStyle is how a comment's markdown is drawn: the window's own
// palette, not the renderer's. Its stock theme is a document's — a body color
// of its own, headings in a blue found nowhere else here, inline code as a red
// chip on a painted field — and a comment in the issue tab is not a document
// beside the window, it is part of it. So the text is the terminal's own, a
// heading is what a title is everywhere else (the accent, bold), code in a
// sentence is the blue a port is in the header, with no field behind it, a
// link is that blue too, and everything secondary is the dim the window
// already uses.
//
// A block of code is highlighted, in discobox-review's colors rather than the
// renderer's: a short list that leaves names, punctuation and whatever no
// lexer claims in the terminal's own text, since a line where everything is
// colored emphasizes nothing. It has no field behind it either.
//
// Without color it is the renderer's plain style, which marks structure with
// characters.
func markdownStyle(st *styles) ansi.StyleConfig {
	if !st.color {
		style := glamourstyles.ASCIIStyleConfig
		return flushDocument(style)
	}
	str := func(s string) *string { return &s }
	yes, no := true, false

	style := glamourstyles.DarkStyleConfig
	style.Document.Color = nil

	heading := ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: str(colGold), Bold: &yes}}
	style.Heading = ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{BlockSuffix: "\n", Color: str(colGold), Bold: &yes}}
	// The levels are told apart by how they sit in the text, not by a row of
	// hashes: a reader of the page never sees those.
	style.H1, style.H2, style.H3 = heading, heading, heading
	minor := ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: str(colGold), Bold: &no}}
	style.H4, style.H5, style.H6 = minor, minor, minor

	style.Code = ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: str(colInfo)}}
	style.CodeBlock.Color = nil
	fg := func(c string) ansi.StylePrimitive { return ansi.StylePrimitive{Color: str(c)} }
	style.CodeBlock.Chroma = &ansi.Chroma{
		Keyword:         fg(colSynKeyword),
		KeywordType:     fg(colSynType),
		NameBuiltin:     fg(colSynType),
		NameClass:       fg(colSynType),
		NameFunction:    fg(colSynFunc),
		NameTag:         fg(colSynFunc),
		NameAttribute:   fg(colSynNumber),
		LiteralNumber:   fg(colSynNumber),
		LiteralString:   fg(colSynString),
		Comment:         fg(colSynComment),
		CommentPreproc:  fg(colSynComment),
		GenericDeleted:  fg(colSynDel),
		GenericInserted: fg(colSynAdd),
		GenericEmph:     ansi.StylePrimitive{Italic: &yes},
		GenericStrong:   ansi.StylePrimitive{Bold: &yes},
	}

	style.Link = ansi.StylePrimitive{Color: str(colInfo)}
	style.LinkText = ansi.StylePrimitive{Color: str(colInfo)}
	style.Image = ansi.StylePrimitive{Color: str(colInfo)}
	style.ImageText = ansi.StylePrimitive{Color: str(colDim), Format: "image: {{.text}}"}

	style.BlockQuote.Color = str(colGrey)
	style.BlockQuote.IndentToken = str("│ ")
	style.HorizontalRule.Color = str(colInactive)
	style.Table.Color = nil
	style.Table.CenterSeparator, style.Table.ColumnSeparator, style.Table.RowSeparator = str("┼"), str("│"), str("─")
	return flushDocument(style)
}

// flushDocument takes the margin and the blank lines the renderer puts round a
// whole document: here the comment's own header already separates it.
func flushDocument(style ansi.StyleConfig) ansi.StyleConfig {
	zero := uint(0)
	style.Document.Margin = &zero
	style.Document.BlockPrefix, style.Document.BlockSuffix = "", ""
	return style
}
