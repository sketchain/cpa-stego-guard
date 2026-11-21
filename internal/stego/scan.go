// Package stego finds the steganographic markers that Claude Code 2.1.91 to
// 2.1.196 writes into the date line of a request when ANTHROPIC_BASE_URL is not
// the official endpoint, and computes the minimal byte edits that remove them.
//
// The marker lives in the first user message, inside a <system-reminder> block:
//
//	Today's date is 2026-10-09.
//
// The apostrophe after "Today" encodes which lists the client matched, and a
// slash date separator (2026/10/09) encodes a China time zone. Everything here
// works on the raw request bytes and never re-serializes JSON, so a body without
// markers is left byte-for-byte unchanged.
package stego

import (
	"bytes"
	"strings"
	"unsafe"

	"github.com/tidwall/gjson"
)

// QuoteKind classifies the apostrophe that follows "Today" on the date line.
type QuoteKind uint8

const (
	// QuoteASCII is U+0027, the unmarked apostrophe.
	QuoteASCII QuoteKind = iota
	// QuoteDomain is U+2019: the base URL matched the domain list.
	QuoteDomain
	// QuoteLab is U+02BC: the base URL matched an AI lab keyword.
	QuoteLab
	// QuoteBoth is U+02B9: both lists matched.
	QuoteBoth
)

// Rune returns the code point that encodes the kind.
func (k QuoteKind) Rune() rune {
	switch k {
	case QuoteDomain:
		return '\u2019'
	case QuoteLab:
		return '\u02BC'
	case QuoteBoth:
		return '\u02B9'
	default:
		return '\''
	}
}

// Marked reports whether the apostrophe is one of the marker code points.
func (k QuoteKind) Marked() bool {
	return k != QuoteASCII
}

func quoteKindOf(r rune) (QuoteKind, bool) {
	switch r {
	case '\'':
		return QuoteASCII, true
	case '\u2019':
		return QuoteDomain, true
	case '\u02BC':
		return QuoteLab, true
	case '\u02B9':
		return QuoteBoth, true
	default:
		return 0, false
	}
}

// Span is a half-open byte range [Start, End) in the request body.
type Span struct {
	Start int
	End   int
}

// DateLine is one "Today's date is ..." line found inside a <system-reminder>
// block of the first user message. All spans are absolute offsets into the
// scanned body and cover the raw JSON encoding of the character, which may be a
// multi-byte UTF-8 sequence or a \uXXXX escape.
type DateLine struct {
	Quote     QuoteKind
	QuoteSpan Span
	// Slash reports a YYYY/MM/DD date, the China time zone marker.
	Slash bool
	// SeparatorSpans are the two date separators when Slash is true.
	SeparatorSpans [2]Span
}

// Report is the result of scanning one request body.
type Report struct {
	// Messages reports that the body has the Anthropic Messages shape.
	Messages bool
	// DateLines lists every date line found, marked or not.
	DateLines []DateLine
	// StrayLab and StrayBoth count U+02BC and U+02B9 anywhere in the body
	// outside the date line apostrophes. They are reported, never edited.
	StrayLab  int
	StrayBoth int
	// Billing is filled by ScanWith with Options.Billing.
	Billing Billing
}

// Counts summarizes a report by marker type.
type Counts struct {
	DateLines   int
	QuoteDomain int
	QuoteLab    int
	QuoteBoth   int
	RegionSlash int
	StrayLab    int
	StrayBoth   int
}

// QuoteMarkers is the number of marked date line apostrophes.
func (c Counts) QuoteMarkers() int {
	return c.QuoteDomain + c.QuoteLab + c.QuoteBoth
}

// Any reports whether the report contains any marker or stray character.
func (c Counts) Any() bool {
	return c.QuoteMarkers() > 0 || c.RegionSlash > 0 || c.StrayLab > 0 || c.StrayBoth > 0
}

// Counts returns the per-type marker counts of the report.
func (r Report) Counts() Counts {
	c := Counts{DateLines: len(r.DateLines), StrayLab: r.StrayLab, StrayBoth: r.StrayBoth}
	for _, line := range r.DateLines {
		switch line.Quote {
		case QuoteDomain:
			c.QuoteDomain++
		case QuoteLab:
			c.QuoteLab++
		case QuoteBoth:
			c.QuoteBoth++
		}
		if line.Slash {
			c.RegionSlash++
		}
	}
	return c
}

var systemReminderProbe = []byte("system-reminder")

// LooksLikeMessages reports whether body has the Anthropic Messages request
// shape. The raw body patch hook receives no protocol metadata, so the shape is
// the only evidence: a top-level object with a messages array, none of the
// top-level fields other protocols use instead (contents, input), and no
// message role that only OpenAI Chat Completions uses.
func LooksLikeMessages(body []byte) bool {
	_, ok := parseMessages(body)
	return ok
}

// messagesDoc holds the parts of a Messages request the scanners read. Every
// result references body.
type messagesDoc struct {
	// firstUser is the first message whose role is user (Type is gjson.Null
	// when there is none).
	firstUser gjson.Result
	// system and metadata are the first top-level members of those names.
	system   gjson.Result
	metadata gjson.Result
}

// parseMessages validates the Messages shape in one pass over the top-level
// object and one pass over the messages array.
func parseMessages(body []byte) (messagesDoc, bool) {
	var doc messagesDoc
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return doc, false
	}
	// No separate validation pass: CLIProxyAPI has already parsed the body,
	// and every edit is confined to a string literal whose bytes rawSpan and
	// decodeLiteral verify. The string view avoids copying a multi-MB body;
	// body is never modified while results that reference it are alive.
	var messages gjson.Result
	otherProtocol := false
	gjson.Parse(unsafe.String(unsafe.SliceData(body), len(body))).ForEach(func(key, value gjson.Result) bool {
		switch key.String() {
		case "messages":
			messages = value
		case "system":
			if !doc.system.Exists() {
				doc.system = value
			}
		case "metadata":
			if !doc.metadata.Exists() {
				doc.metadata = value
			}
		case "contents", "input":
			otherProtocol = true
			return false
		}
		return true
	})
	if otherProtocol || !messages.IsArray() || !strings.HasSuffix(messages.Raw, "]") {
		return messagesDoc{}, false
	}
	openAIRole := false
	messages.ForEach(func(_, message gjson.Result) bool {
		role := message.Get("role").String()
		switch role {
		case "developer", "tool", "function":
			openAIRole = true
			return false
		case "user":
			if doc.firstUser.Type == gjson.Null {
				doc.firstUser = message
			}
		}
		return true
	})
	if openAIRole {
		return messagesDoc{}, false
	}
	return doc, true
}

// Options selects optional work for ScanWith.
type Options struct {
	// Billing also reads the Claude Code billing header, the first user text
	// that seeds its fingerprint, and metadata.user_id (Report.Billing).
	Billing bool
}

// Scan inspects an Anthropic Messages request body for date line markers. It
// returns a report with Messages false, and nothing else, when the body does
// not have that shape.
func Scan(body []byte) Report {
	return ScanWith(body, Options{})
}

// ScanWith is Scan with optional extra work, sharing one parse of the body.
func ScanWith(body []byte, opts Options) Report {
	var report Report
	doc, ok := parseMessages(body)
	if !ok {
		return report
	}
	report.Messages = true
	if doc.firstUser.Type != gjson.Null && bytes.Contains(body, systemReminderProbe) {
		for _, text := range firstUserTexts(body, doc.firstUser) {
			report.DateLines = append(report.DateLines, scanText(body, text)...)
		}
	}
	report.StrayLab, report.StrayBoth = countStrayModifiers(body, report.DateLines)
	if opts.Billing {
		report.Billing = scanBilling(body, doc)
	}
	return report
}

// firstUserTexts returns the raw JSON string literals holding the text of the
// first user message: the content itself when it is a string, otherwise the
// text of every text block. Each span covers the literal including its quotes.
func firstUserTexts(body []byte, message gjson.Result) []Span {
	content := message.Get("content")
	if content.Type == gjson.String {
		if span, ok := rawSpan(body, content); ok {
			return []Span{span}
		}
		return nil
	}
	if !content.IsArray() {
		return nil
	}
	var spans []Span
	content.ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() != "text" {
			return true
		}
		text := block.Get("text")
		if text.Type != gjson.String {
			return true
		}
		if span, ok := rawSpan(body, text); ok {
			spans = append(spans, span)
		}
		return true
	})
	return spans
}

// rawSpan locates a string result in body. gjson reports the offset of results
// read straight from the input; the bytes are compared anyway so an offset that
// does not point at the literal can never produce an edit.
func rawSpan(body []byte, result gjson.Result) (Span, bool) {
	start := result.Index
	end := start + len(result.Raw)
	if start <= 0 || end > len(body) || len(result.Raw) < 2 || string(body[start:end]) != result.Raw {
		return Span{}, false
	}
	return Span{Start: start, End: end}, true
}

const (
	reminderOpen  = "<system-reminder>"
	reminderClose = "</system-reminder>"
	dateLead      = "Today"
	dateTail      = "s date is "
)

// scanText finds date lines inside <system-reminder> blocks of one JSON string
// literal and returns them with absolute body offsets.
func scanText(body []byte, literal Span) []DateLine {
	text, offsets, ok := decodeLiteral(body[literal.Start:literal.End])
	if !ok {
		return nil
	}
	var lines []DateLine
	for from := 0; ; {
		open := indexRunes(text, reminderOpen, from)
		if open < 0 {
			break
		}
		bodyStart := open + len(reminderOpen)
		end := indexRunes(text, reminderClose, bodyStart)
		if end < 0 {
			break
		}
		for i := bodyStart; i < end; i++ {
			if text[i-1] != '\n' || !hasRunes(text, i, dateLead) {
				continue
			}
			line, ok := matchDateLine(text, i, end)
			if !ok {
				continue
			}
			lines = append(lines, line.absolute(literal.Start, offsets))
		}
		from = end + len(reminderClose)
	}
	return lines
}

// relativeDateLine holds rune indexes into a decoded literal.
type relativeDateLine struct {
	quote      QuoteKind
	quoteIndex int
	slash      bool
	separators [2]int
}

func (l relativeDateLine) absolute(base int, offsets []int) DateLine {
	span := func(i int) Span {
		return Span{Start: base + offsets[i], End: base + offsets[i+1]}
	}
	line := DateLine{Quote: l.quote, QuoteSpan: span(l.quoteIndex), Slash: l.slash}
	if l.slash {
		line.SeparatorSpans = [2]Span{span(l.separators[0]), span(l.separators[1])}
	}
	return line
}

// matchDateLine matches "Today?s date is YYYY?MM?DD" starting at rune index i,
// where the apostrophe is one of the four known code points and both date
// separators are the same character, either '-' or '/'. limit is the index of
// the closing reminder tag.
func matchDateLine(text []rune, i, limit int) (relativeDateLine, bool) {
	var line relativeDateLine
	q := i + len(dateLead)
	if q >= limit {
		return line, false
	}
	kind, ok := quoteKindOf(text[q])
	if !ok || !hasRunes(text, q+1, dateTail) {
		return line, false
	}
	d := q + 1 + len(dateTail)
	// YYYY S MM S DD
	if d+10 > limit || !digits(text[d:d+4]) || !digits(text[d+5:d+7]) || !digits(text[d+8:d+10]) {
		return line, false
	}
	sep := text[d+4]
	if (sep != '-' && sep != '/') || text[d+7] != sep {
		return line, false
	}
	if d+10 < limit && isDigit(text[d+10]) {
		return line, false
	}
	month := int(text[d+5]-'0')*10 + int(text[d+6]-'0')
	day := int(text[d+8]-'0')*10 + int(text[d+9]-'0')
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return line, false
	}
	line.quote = kind
	line.quoteIndex = q
	if sep == '/' {
		line.slash = true
		line.separators = [2]int{d + 4, d + 7}
	}
	return line, true
}

func digits(runes []rune) bool {
	for _, r := range runes {
		if !isDigit(r) {
			return false
		}
	}
	return true
}

func isDigit(r rune) bool {
	return r >= '0' && r <= '9'
}

func hasRunes(text []rune, at int, want string) bool {
	if at < 0 {
		return false
	}
	i := at
	for _, r := range want {
		if i >= len(text) || text[i] != r {
			return false
		}
		i++
	}
	return true
}

func indexRunes(text []rune, want string, from int) int {
	for i := from; i < len(text); i++ {
		if hasRunes(text, i, want) {
			return i
		}
	}
	return -1
}

// countStrayModifiers counts U+02BC and U+02B9 in the raw body, both as UTF-8
// and as \u escapes, skipping the date line apostrophes already reported.
func countStrayModifiers(body []byte, lines []DateLine) (lab, both int) {
	skip := make(map[int]struct{}, len(lines))
	for _, line := range lines {
		if line.Quote == QuoteLab || line.Quote == QuoteBoth {
			skip[line.QuoteSpan.Start] = struct{}{}
		}
	}
	tally := func(at int, low byte) {
		if _, ok := skip[at]; ok {
			return
		}
		switch low {
		case 0xBC:
			lab++
		case 0xB9:
			both++
		}
	}
	// UTF-8: U+02BC is CA BC and U+02B9 is CA B9.
	for from := 0; ; {
		i := bytes.IndexByte(body[from:], 0xCA)
		if i < 0 || from+i+1 >= len(body) {
			break
		}
		at := from + i
		tally(at, body[at+1])
		from = at + 1
	}
	// Escapes: \u02bc and \u02b9 with either hex digit case.
	escape := []byte(`\u02`)
	for from := 0; ; {
		i := bytes.Index(body[from:], escape)
		if i < 0 || from+i+len(escape)+2 > len(body) {
			break
		}
		at := from + i
		hex := body[at+len(escape) : at+len(escape)+2]
		if (hex[0] == 'b' || hex[0] == 'B') && !escapedBackslash(body, at) {
			switch hex[1] {
			case 'c', 'C':
				tally(at, 0xBC)
			case '9':
				tally(at, 0xB9)
			}
		}
		from = at + 1
	}
	return lab, both
}

// escapedBackslash reports whether the backslash at i is itself escaped, as in
// the JSON text \\u02bc, which encodes a literal backslash and not U+02BC.
func escapedBackslash(body []byte, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && body[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}
