package chatwoot

import (
	"regexp"
	"strings"
)

// WhatsApp and Chatwoot (Markdown) mark text up differently:
//
//	           WhatsApp   Chatwoot
//	bold       *x*        **x**
//	italic     _x_        *x*
//	strike     ~x~        ~~x~~
//	monospace  ```x```    `x`
//
// The content of a marker must start and end with a non-space character and
// stay on one line, as both apps require.

const (
	inner = `[^\s%[1]s](?:[^\n%[1]s]*[^\s%[1]s])?`

	// Converted markers are written as placeholders and replaced at the end,
	// so no rule ever re-reads the output of another (or of itself).
	boldMark   = "\x00"
	monoMark   = "\x01"
	tripleTag  = "\x02"
	italicMark = "\x03"
	strikeMark = "\x04"
)

var (
	// WhatsApp only formats markers that are not glued to letters or digits,
	// so "snake_case_name" stays as is.
	waBold   = waMarker(`*`)
	waItalic = waMarker(`_`)
	waStrike = waMarker(`~`)
	waMono   = regexp.MustCompile("```([^`\n]+)```")

	cwBold   = regexp.MustCompile(`\*\*(` + sprintfInner(`*`) + `)\*\*`)
	cwItalic = regexp.MustCompile(`\*(` + sprintfInner(`*`) + `)\*`)
	cwStrike = regexp.MustCompile(`~~(` + sprintfInner(`~`) + `)~~`)
	cwMono   = regexp.MustCompile("`([^`\n]+)`")
)

// stripPlaceholders removes the placeholder characters from user text.
var stripPlaceholders = strings.NewReplacer(boldMark, "", monoMark, "", tripleTag, "", italicMark, "", strikeMark, "")

func sprintfInner(marker string) string {
	return strings.ReplaceAll(inner, "%[1]s", regexp.QuoteMeta(marker))
}

func waMarker(marker string) *regexp.Regexp {
	m := regexp.QuoteMeta(marker)
	return regexp.MustCompile(`(^|[^\pL\pN])` + m + `(` + sprintfInner(marker) + `)` + m + `($|[^\pL\pN])`)
}

// replaceWA applies a WhatsApp marker rule. The boundary characters are part of
// each match, so "*a* *b*" needs a second pass for the marker after the space.
func replaceWA(re *regexp.Regexp, s, open, close string) string {
	repl := "${1}" + open + "${2}" + close + "${3}"
	return re.ReplaceAllString(re.ReplaceAllString(s, repl), repl)
}

// WhatsAppToChatwoot converts WhatsApp formatting to Markdown.
func WhatsAppToChatwoot(s string) string {
	if s == "" {
		return s
	}
	s = stripPlaceholders.Replace(s)
	s = waMono.ReplaceAllString(s, monoMark+"${1}"+monoMark)
	s = replaceWA(waBold, s, boldMark, boldMark)
	s = replaceWA(waItalic, s, italicMark, italicMark)
	s = replaceWA(waStrike, s, strikeMark, strikeMark)
	return strings.NewReplacer(boldMark, "**", italicMark, "*", strikeMark, "~~", monoMark, "`").Replace(s)
}

// ChatwootToWhatsApp converts Markdown formatting to WhatsApp.
func ChatwootToWhatsApp(s string) string {
	if s == "" {
		return s
	}
	s = stripPlaceholders.Replace(s)
	// Code blocks are already WhatsApp monospace; protect them from the
	// single-backtick rule.
	s = strings.ReplaceAll(s, "```", tripleTag)
	s = cwMono.ReplaceAllString(s, monoMark+"${1}"+monoMark)
	s = cwBold.ReplaceAllString(s, boldMark+"${1}"+boldMark)
	s = cwItalic.ReplaceAllString(s, italicMark+"${1}"+italicMark)
	s = cwStrike.ReplaceAllString(s, strikeMark+"${1}"+strikeMark)
	return strings.NewReplacer(boldMark, "*", italicMark, "_", strikeMark, "~", monoMark, "```", tripleTag, "```").Replace(s)
}
