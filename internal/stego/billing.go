package stego

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"unicode/utf16"

	"github.com/tidwall/gjson"
)

// BillingPrefix starts the system text block in which Claude Code reports its
// version: "x-anthropic-billing-header: cc_version=2.1.321.20d; cc_entrypoint=cli;".
const BillingPrefix = "x-anthropic-billing-header:"

const (
	ccVersionKey = "cc_version="
	// fingerprintSalt and the sampled UTF-16 indexes are Claude Code's build
	// fingerprint parameters, the three hex digits after the version.
	fingerprintSalt = "59cf53e54c78"
	// fingerprintLen is the number of hex digits Claude Code keeps.
	fingerprintLen = 3
	// dateReminderPrefix starts the currentDate reminder block that Claude
	// Code puts before the user's text.
	dateReminderPrefix = "<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# currentDate\nToday's date is "
)

var fingerprintIndexes = [...]int{4, 7, 20}

// Billing describes the Claude Code billing header of a Messages request.
type Billing struct {
	// Blocks lists every system text block whose text starts with
	// BillingPrefix, in order.
	Blocks []BillingBlock
	// BlockDeletes are the byte ranges that remove all of Blocks from the
	// system array, commas included, so the array stays valid JSON.
	BlockDeletes []Span
	// UserIDDeletes are the byte ranges that remove every metadata.user_id
	// member from the metadata object, commas included.
	UserIDDeletes []Span
	// FingerprintText is the text Claude Code hashes into the fingerprint: the
	// first user message's content when it is a string, otherwise its first
	// text block that is not a <system-reminder> block. FingerprintKnown is
	// false when that text cannot be determined the way CLIProxyAPI does.
	FingerprintText  []rune
	FingerprintKnown bool
}

// BillingBlock is one billing header text block.
type BillingBlock struct {
	// Parsed reports that the text carries cc_version=<x.y.z>.<3 hex>;.
	Parsed bool
	// Version and Fingerprint are the two parts of cc_version.
	Version     string
	Fingerprint string
	// ValueSpan covers "<version>.<fingerprint>" in the body.
	ValueSpan Span
}

// Fingerprint computes Claude Code's 3-digit build fingerprint of text for a
// version: the first three hex digits of SHA-256(salt + text[4] + text[7] +
// text[20] + version), where text is indexed in UTF-16 code units, missing
// positions read as '0', and an unpaired surrogate is encoded as U+FFFD.
func Fingerprint(text, version string) string {
	return fingerprintRunes([]rune(text), version)
}

func fingerprintRunes(text []rune, version string) string {
	// utf16.Encode writes U+FFFD for runes that are themselves surrogates,
	// which is how JavaScript's UTF-8 encoder treats an unpaired one.
	units := utf16.Encode(text)
	var sampled [len(fingerprintIndexes)]uint16
	for i, at := range fingerprintIndexes {
		sampled[i] = '0'
		if at < len(units) {
			sampled[i] = units[at]
		}
	}
	// Decode pairs two sampled halves that happen to form a surrogate pair
	// and turns every other surrogate into U+FFFD, as JavaScript string
	// concatenation followed by UTF-8 encoding does.
	input := fingerprintSalt + string(utf16.Decode(sampled[:])) + version
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])[:fingerprintLen]
}

// ValidVersion reports whether v is a dotted three-part decimal version such
// as 2.1.321.
func ValidVersion(v string) bool {
	runes := []rune(v)
	end, ok := matchVersion(runes, 0)
	return ok && end == len(runes)
}

// matchVersion matches digits.digits.digits at i and returns its end.
func matchVersion(text []rune, i int) (int, bool) {
	for part := 0; part < 3; part++ {
		if part > 0 {
			if i >= len(text) || text[i] != '.' {
				return i, false
			}
			i++
		}
		start := i
		for i < len(text) && isDigit(text[i]) {
			i++
		}
		if i == start {
			return i, false
		}
	}
	return i, true
}

func isLowerHex(r rune) bool {
	return isDigit(r) || (r >= 'a' && r <= 'f')
}

// scanBilling reads the billing blocks, the fingerprint text and the
// metadata.user_id members of a Messages request.
func scanBilling(body []byte, doc messagesDoc) Billing {
	var billing Billing
	if doc.system.IsArray() {
		billing.Blocks, billing.BlockDeletes = scanBillingBlocks(body, doc.system)
	}
	if len(billing.Blocks) == 0 {
		return billing
	}
	billing.FingerprintText, billing.FingerprintKnown = fingerprintText(body, doc.firstUser)
	if doc.metadata.IsObject() {
		billing.UserIDDeletes = memberDeletes(body, doc.metadata, func(key string) bool { return key == "user_id" })
	}
	return billing
}

func scanBillingBlocks(body []byte, system gjson.Result) ([]BillingBlock, []Span) {
	span, ok := rawSpan(body, system)
	if !ok {
		return nil, nil
	}
	elements, ok := containerElements(body, span)
	if !ok {
		return nil, nil
	}
	var blocks []BillingBlock
	remove := make([]bool, len(elements))
	for i, element := range elements {
		value := element.value
		if body[value.Start] != '{' {
			continue
		}
		item := gjson.Parse(string(body[value.Start:value.End]))
		if item.Get("type").String() != "text" {
			continue
		}
		text := item.Get("text")
		if text.Type != gjson.String {
			continue
		}
		literal, ok := rawSpan(body[value.Start:value.End], text)
		if !ok {
			continue
		}
		literal.Start += value.Start
		literal.End += value.Start
		runes, offsets, ok := decodeLiteral(body[literal.Start:literal.End])
		if !ok || !hasRunes(runes, 0, BillingPrefix) {
			continue
		}
		block := parseBillingText(runes)
		if block.Parsed {
			start, end := block.ValueSpan.Start, block.ValueSpan.End
			block.ValueSpan = Span{Start: literal.Start + offsets[start], End: literal.Start + offsets[end]}
		}
		blocks = append(blocks, block)
		remove[i] = true
	}
	return blocks, deleteRuns(elements, remove)
}

// parseBillingText finds cc_version=<x.y.z>.<hex3>; in a decoded billing
// header. ValueSpan holds rune indexes of "<x.y.z>.<hex3>".
func parseBillingText(text []rune) BillingBlock {
	var block BillingBlock
	key := indexRunes(text, ccVersionKey, len([]rune(BillingPrefix)))
	if key < 0 {
		return block
	}
	start := key + len(ccVersionKey)
	versionEnd, ok := matchVersion(text, start)
	if !ok || versionEnd >= len(text) || text[versionEnd] != '.' {
		return block
	}
	fpStart := versionEnd + 1
	fpEnd := fpStart + fingerprintLen
	if fpEnd >= len(text) || text[fpEnd] != ';' {
		return block
	}
	for _, r := range text[fpStart:fpEnd] {
		if !isLowerHex(r) {
			return block
		}
	}
	block.Parsed = true
	block.Version = string(text[start:versionEnd])
	block.Fingerprint = string(text[fpStart:fpEnd])
	block.ValueSpan = Span{Start: start, End: fpEnd}
	return block
}

// fingerprintText selects the text CLIProxyAPI's billing fingerprint uses:
// string content as is; otherwise the first text block that is neither the
// currentDate reminder nor a <system-reminder>...</system-reminder> block. A
// text block with a missing or null text yields "". A text value of any other
// non-string type, or an undecodable literal, is reported as unknown.
func fingerprintText(body []byte, firstUser gjson.Result) ([]rune, bool) {
	if firstUser.Type == gjson.Null {
		return nil, true
	}
	content := firstUser.Get("content")
	if content.Type == gjson.String {
		return decodeResult(body, content)
	}
	if !content.IsArray() {
		return nil, true
	}
	var (
		text  []rune
		known = true
	)
	content.ForEach(func(_, part gjson.Result) bool {
		if part.Get("type").String() != "text" {
			return true
		}
		value := part.Get("text")
		switch value.Type {
		case gjson.String:
			runes, ok := decodeResult(body, value)
			if !ok {
				known = false
				return false
			}
			if isReminderBlock(runes) {
				return true
			}
			text = runes
			return false
		case gjson.Null:
			// Missing or null text reads as "" and ends the search.
			return false
		default:
			known = false
			return false
		}
	})
	if !known {
		return nil, false
	}
	return text, true
}

func decodeResult(body []byte, value gjson.Result) ([]rune, bool) {
	span, ok := rawSpan(body, value)
	if !ok {
		return nil, false
	}
	runes, _, ok := decodeLiteral(body[span.Start:span.End])
	return runes, ok
}

func isReminderBlock(text []rune) bool {
	if hasRunes(text, 0, dateReminderPrefix) {
		return true
	}
	return hasRunes(text, 0, reminderOpen) && indexRunes(text, reminderClose, 0) >= 0
}

// element is one member of a JSON array or object: span covers the key (for
// objects) through the end of the value.
type element struct {
	span  Span
	key   string
	value Span
}

// containerElements splits the array or object at span into its members.
func containerElements(body []byte, span Span) ([]element, bool) {
	s := jsonScanner{body: body[:span.End], pos: span.Start}
	open := s.body[s.pos]
	if open != '[' && open != '{' {
		return nil, false
	}
	closing := byte(']')
	if open == '{' {
		closing = '}'
	}
	s.pos++
	var elements []element
	s.skipSpace()
	if s.peek() == closing {
		return elements, s.pos == span.End-1
	}
	for {
		s.skipSpace()
		var e element
		e.span.Start = s.pos
		if open == '{' {
			keyStart := s.pos
			if !s.skipString() {
				return nil, false
			}
			runes, _, ok := decodeLiteral(s.body[keyStart:s.pos])
			if !ok {
				return nil, false
			}
			e.key = string(runes)
			s.skipSpace()
			if s.peek() != ':' {
				return nil, false
			}
			s.pos++
			s.skipSpace()
		}
		e.value.Start = s.pos
		if !s.skipValue() {
			return nil, false
		}
		e.value.End = s.pos
		e.span.End = s.pos
		elements = append(elements, e)
		s.skipSpace()
		switch s.peek() {
		case ',':
			s.pos++
		case closing:
			return elements, s.pos == span.End-1
		default:
			return nil, false
		}
	}
}

// memberDeletes returns the ranges that remove the object members whose key
// matches, keeping the object valid JSON.
func memberDeletes(body []byte, object gjson.Result, match func(string) bool) []Span {
	span, ok := rawSpan(body, object)
	if !ok {
		return nil
	}
	elements, ok := containerElements(body, span)
	if !ok {
		return nil
	}
	remove := make([]bool, len(elements))
	for i, e := range elements {
		remove[i] = match(e.key)
	}
	return deleteRuns(elements, remove)
}

// deleteRuns turns a per-element removal mask into non-overlapping byte
// ranges. A run of removed elements followed by a kept one is cut up to that
// element's start, which takes the trailing commas; a run that reaches the end
// is cut from the previous kept element's end, which takes the leading comma.
func deleteRuns(elements []element, remove []bool) []Span {
	var spans []Span
	for i := 0; i < len(elements); {
		if !remove[i] {
			i++
			continue
		}
		j := i
		for j+1 < len(elements) && remove[j+1] {
			j++
		}
		switch {
		case j+1 < len(elements):
			spans = append(spans, Span{Start: elements[i].span.Start, End: elements[j+1].span.Start})
		case i > 0:
			spans = append(spans, Span{Start: elements[i-1].span.End, End: elements[j].span.End})
		default:
			spans = append(spans, Span{Start: elements[i].span.Start, End: elements[j].span.End})
		}
		i = j + 1
	}
	return spans
}

// jsonScanner skips over JSON values in an already validated body.
type jsonScanner struct {
	body []byte
	pos  int
}

func (s *jsonScanner) peek() byte {
	if s.pos >= len(s.body) {
		return 0
	}
	return s.body[s.pos]
}

func (s *jsonScanner) skipSpace() {
	for s.pos < len(s.body) {
		switch s.body[s.pos] {
		case ' ', '\t', '\n', '\r':
			s.pos++
		default:
			return
		}
	}
}

func (s *jsonScanner) skipString() bool {
	if s.peek() != '"' {
		return false
	}
	for s.pos++; s.pos < len(s.body); s.pos++ {
		switch s.body[s.pos] {
		case '\\':
			s.pos++
		case '"':
			s.pos++
			return true
		}
	}
	return false
}

func (s *jsonScanner) skipValue() bool {
	switch s.peek() {
	case '"':
		return s.skipString()
	case '[', '{':
		depth := 0
		for s.pos < len(s.body) {
			switch s.body[s.pos] {
			case '"':
				if !s.skipString() {
					return false
				}
				continue
			case '[', '{':
				depth++
			case ']', '}':
				depth--
				if depth == 0 {
					s.pos++
					return true
				}
			}
			s.pos++
		}
		return false
	default:
		start := s.pos
		for s.pos < len(s.body) {
			switch s.body[s.pos] {
			case ',', ']', '}', ' ', '\t', '\n', '\r':
				return s.pos > start
			}
			s.pos++
		}
		return s.pos > start
	}
}

// CCKind is what aligning cc_version does to a request.
type CCKind string

const (
	// CCNone leaves the request alone: alignment is off, there is no billing
	// header, or every cc_version already equals the target and carries a
	// valid fingerprint.
	CCNone CCKind = "none"
	// CCAlign rewrites cc_version to the target version and its fingerprint.
	CCAlign CCKind = "align"
	// CCFallback removes the billing header and metadata.user_id, so that
	// CLIProxyAPI does not confirm the request as Claude Code, cloaks it and
	// writes its own billing header.
	CCFallback CCKind = "fallback"
)

// CC fallback reasons, as logged.
const (
	ReasonUnrecognized       = "unrecognized_billing_header"
	ReasonFingerprintUnknown = "fingerprint_text_unknown"
	ReasonFingerprintInvalid = "fingerprint_mismatch"
)

// CCResult is the outcome of aligning cc_version.
type CCResult struct {
	Kind CCKind
	// From is the client's cc_version: the first one that differs from the
	// target, else the target itself when a block at the target version fails
	// verification; empty when the billing header could not be read.
	From string
	// Reason explains a fallback.
	Reason string
	// Edits are ordered by offset and do not overlap.
	Edits []Edit
}

// Edit replaces Span with Replacement.
type Edit struct {
	Span
	Replacement string
}

// AlignCCVersion decides how to bring the billing header of a request to the
// target version. It is a pure function of its inputs. Every cc_version must
// carry the fingerprint Claude Code computes for its own version, including
// one that already equals target. A verified cc_version equal to target is
// left alone; any other verified one is rewritten to target with the
// fingerprint recomputed for target, and cch is left for CLIProxyAPI to
// re-sign. If any billing block cannot be read or verified, the result is a
// fallback that removes all billing blocks and metadata.user_id. Applying the
// result and aligning again yields CCNone.
func AlignCCVersion(billing Billing, target string) CCResult {
	result := CCResult{Kind: CCNone}
	if target == "" || len(billing.Blocks) == 0 {
		return result
	}
	var edits []Edit
	for _, block := range billing.Blocks {
		atTarget := block.Parsed && block.Version == target
		if !atTarget && result.From == "" {
			result.From = block.Version
		}
		var reason string
		switch {
		case !block.Parsed:
			reason = ReasonUnrecognized
		case !billing.FingerprintKnown:
			reason = ReasonFingerprintUnknown
		case fingerprintRunes(billing.FingerprintText, block.Version) != block.Fingerprint:
			reason = ReasonFingerprintInvalid
		}
		if reason != "" {
			from := result.From
			if from == "" {
				from = block.Version
			}
			return fallback(billing, from, reason)
		}
		if atTarget {
			continue
		}
		edits = append(edits, Edit{
			Span:        block.ValueSpan,
			Replacement: target + "." + fingerprintRunes(billing.FingerprintText, target),
		})
	}
	if len(edits) == 0 {
		return result
	}
	result.Kind = CCAlign
	result.Edits = edits
	return result
}

func fallback(billing Billing, from, reason string) CCResult {
	result := CCResult{Kind: CCFallback, From: from, Reason: reason}
	for _, span := range billing.BlockDeletes {
		result.Edits = append(result.Edits, Edit{Span: span})
	}
	for _, span := range billing.UserIDDeletes {
		result.Edits = append(result.Edits, Edit{Span: span})
	}
	sort.Slice(result.Edits, func(i, j int) bool { return result.Edits[i].Start < result.Edits[j].Start })
	return result
}
