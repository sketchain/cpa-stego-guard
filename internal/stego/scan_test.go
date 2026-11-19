package stego

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

const (
	reminderHead = "<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# claudeMd\nCodebase and user instructions are shown below.\n\n# currentDate\n"
	reminderTail = "\n\n      IMPORTANT: this context may or may not be relevant to your tasks. You should not respond to this context unless it is highly relevant to your task.\n</system-reminder>\n"
	systemPrompt = "You are Claude Code, Anthropic\u2019s official CLI for Claude. If it\u2019s unclear, ask."
)

// fixture builds a Claude Code shaped request body. dateLine is the full line
// inside the reminder, for example "Today's date is 2026-10-09.".
type fixture struct {
	dateLine     string
	stringForm   bool
	escapeHTML   bool
	userText     string
	extraSystem  string
	secondUser   string
	extraMessage map[string]any
}

func (f fixture) build(t *testing.T) []byte {
	t.Helper()
	reminder := reminderHead + f.dateLine + reminderTail
	userText := f.userText
	if userText == "" {
		userText = "Please fix the failing test."
	}
	var content any
	if f.stringForm {
		content = reminder + userText
	} else {
		content = []any{
			map[string]any{"type": "text", "text": reminder},
			map[string]any{"type": "text", "text": userText, "cache_control": map[string]any{"type": "ephemeral"}},
		}
	}
	messages := []any{map[string]any{"role": "user", "content": content}}
	if f.secondUser != "" {
		messages = append(messages,
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "OK."}}},
			map[string]any{"role": "user", "content": f.secondUser},
		)
	}
	if f.extraMessage != nil {
		messages = append(messages, f.extraMessage)
	}
	system := []any{
		map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.150.a1b; cc_entrypoint=cli;"},
		map[string]any{"type": "text", "text": systemPrompt + f.extraSystem, "cache_control": map[string]any{"type": "ephemeral"}},
	}
	body := map[string]any{
		"model":      "claude-sonnet-5-5",
		"max_tokens": 32000,
		"messages":   messages,
		"system":     system,
		"tools":      []any{map[string]any{"name": "Bash", "description": "Run a command", "input_schema": map[string]any{"type": "object"}}},
		"metadata":   map[string]any{"user_id": `{"device_id":"d","account_uuid":"a","session_id":"s"}`},
		"stream":     true,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(f.escapeHTML)
	if err := enc.Encode(body); err != nil {
		t.Fatal(err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// apply splices edits the way CLIProxyAPI's applyRequestBodyEdits does.
func apply(t *testing.T, body []byte, edits []Span, replacements []string) []byte {
	t.Helper()
	type edit struct {
		span Span
		repl string
	}
	list := make([]edit, len(edits))
	for i := range edits {
		list[i] = edit{edits[i], replacements[i]}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].span.Start < list[j].span.Start })
	var out []byte
	last := 0
	for _, e := range list {
		if e.span.Start < last {
			t.Fatalf("overlapping edit at %d", e.span.Start)
		}
		out = append(out, body[last:e.span.Start]...)
		out = append(out, e.repl...)
		last = e.span.End
	}
	return append(out, body[last:]...)
}

// cleanAll removes every date line marker in a report.
func cleanAll(t *testing.T, body []byte, report Report) []byte {
	t.Helper()
	var spans []Span
	var repls []string
	for _, line := range report.DateLines {
		if line.Quote.Marked() {
			spans = append(spans, line.QuoteSpan)
			repls = append(repls, "'")
		}
		if line.Slash {
			spans = append(spans, line.SeparatorSpans[0], line.SeparatorSpans[1])
			repls = append(repls, "-", "-")
		}
	}
	return apply(t, body, spans, repls)
}

func TestScanCleanBody(t *testing.T) {
	body := fixture{dateLine: "Today's date is 2026-10-09."}.build(t)
	report := Scan(body)
	if !report.Messages {
		t.Fatal("expected Messages shape")
	}
	if len(report.DateLines) != 1 {
		t.Fatalf("date lines = %d, want 1", len(report.DateLines))
	}
	c := report.Counts()
	if c.Any() {
		t.Fatalf("clean body reported markers: %+v", c)
	}
	if got := cleanAll(t, body, report); !bytes.Equal(got, body) {
		t.Fatal("clean body changed")
	}
}

func TestScanQuoteMarkers(t *testing.T) {
	cases := []struct {
		name  string
		quote string
		kind  QuoteKind
	}{
		{"domain U+2019", "\u2019", QuoteDomain},
		{"lab U+02BC", "\u02BC", QuoteLab},
		{"both U+02B9", "\u02B9", QuoteBoth},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fixture{dateLine: "Today" + tc.quote + "s date is 2026-10-09."}.build(t)
			report := Scan(body)
			if len(report.DateLines) != 1 {
				t.Fatalf("date lines = %d", len(report.DateLines))
			}
			line := report.DateLines[0]
			if line.Quote != tc.kind || line.Slash {
				t.Fatalf("line = %+v", line)
			}
			if got := string(body[line.QuoteSpan.Start:line.QuoteSpan.End]); got != tc.quote {
				t.Fatalf("quote span covers %q", got)
			}
			if report.StrayLab != 0 || report.StrayBoth != 0 {
				t.Fatalf("date line apostrophe counted as stray: %+v", report.Counts())
			}
			cleaned := cleanAll(t, body, report)
			want := fixture{dateLine: "Today's date is 2026-10-09."}.build(t)
			if !bytes.Equal(cleaned, want) {
				t.Fatalf("cleaned body differs from clean fixture\n got: %s\nwant: %s", cleaned, want)
			}
			// The system prompt keeps its own U+2019.
			if !bytes.Contains(cleaned, []byte("Anthropic\u2019s")) || !bytes.Contains(cleaned, []byte("it\u2019s")) {
				t.Fatal("system prompt apostrophes were changed")
			}
		})
	}
}

func TestScanRegionMarker(t *testing.T) {
	body := fixture{dateLine: "Today's date is 2026/10/09."}.build(t)
	report := Scan(body)
	c := report.Counts()
	if c.RegionSlash != 1 || c.QuoteMarkers() != 0 {
		t.Fatalf("counts = %+v", c)
	}
	cleaned := cleanAll(t, body, report)
	if want := (fixture{dateLine: "Today's date is 2026-10-09."}).build(t); !bytes.Equal(cleaned, want) {
		t.Fatalf("cleaned = %s", cleaned)
	}
}

func TestScanCombinedMarkers(t *testing.T) {
	for _, quote := range []string{"\u2019", "\u02BC", "\u02B9"} {
		body := fixture{dateLine: "Today" + quote + "s date is 2026/10/09."}.build(t)
		report := Scan(body)
		c := report.Counts()
		if c.QuoteMarkers() != 1 || c.RegionSlash != 1 {
			t.Fatalf("%U counts = %+v", []rune(quote)[0], c)
		}
		cleaned := cleanAll(t, body, report)
		if want := (fixture{dateLine: "Today's date is 2026-10-09."}).build(t); !bytes.Equal(cleaned, want) {
			t.Fatalf("cleaned = %s", cleaned)
		}
	}
}

func TestScanStringContent(t *testing.T) {
	body := fixture{dateLine: "Today\u2019s date is 2026/01/31.", stringForm: true}.build(t)
	report := Scan(body)
	c := report.Counts()
	if c.QuoteDomain != 1 || c.RegionSlash != 1 {
		t.Fatalf("counts = %+v", c)
	}
	cleaned := cleanAll(t, body, report)
	if want := (fixture{dateLine: "Today's date is 2026-01-31.", stringForm: true}).build(t); !bytes.Equal(cleaned, want) {
		t.Fatalf("cleaned = %s", cleaned)
	}
}

func TestScanEscapedEncodings(t *testing.T) {
	// Some serializers escape non-ASCII, '/', and HTML characters.
	body := fixture{dateLine: "Today\u02B9s date is 2026/10/09.", escapeHTML: true}.build(t)
	body = bytes.ReplaceAll(body, []byte("\u02B9"), []byte(`\u02B9`))
	body = bytes.ReplaceAll(body, []byte("2026/10/09"), []byte(`2026\/10\/09`))
	if !bytes.Contains(body, []byte(`\u003csystem-reminder\u003e`)) {
		t.Fatal("fixture should escape HTML characters")
	}
	report := Scan(body)
	c := report.Counts()
	if c.QuoteBoth != 1 || c.RegionSlash != 1 || c.StrayBoth != 0 {
		t.Fatalf("counts = %+v", c)
	}
	line := report.DateLines[0]
	if got := string(body[line.QuoteSpan.Start:line.QuoteSpan.End]); got != `\u02B9` {
		t.Fatalf("quote span = %q", got)
	}
	if got := string(body[line.SeparatorSpans[0].Start:line.SeparatorSpans[0].End]); got != `\/` {
		t.Fatalf("separator span = %q", got)
	}
	cleaned := cleanAll(t, body, report)
	if !bytes.Contains(cleaned, []byte("Today's date is 2026-10-09.")) {
		t.Fatalf("cleaned = %s", cleaned)
	}
	if !json.Valid(cleaned) {
		t.Fatal("cleaned body is not valid JSON")
	}
}

func TestScanIgnoresDateLinesOutsideScope(t *testing.T) {
	marked := "Today\u2019s date is 2026/10/09."
	cases := map[string]fixture{
		// The real reminder is clean; marked text appears elsewhere.
		"system prompt":             {dateLine: "Today's date is 2026-10-09.", extraSystem: "\n<system-reminder>\n" + marked + "\n</system-reminder>"},
		"user text outside tag":     {dateLine: "Today's date is 2026-10-09.", userText: "Note:\n" + marked},
		"second user message":       {dateLine: "Today's date is 2026-10-09.", secondUser: "<system-reminder>\n" + marked + "\n</system-reminder>"},
		"not at line start":         {dateLine: "Note: " + marked},
		"invalid month":             {dateLine: "Today\u2019s date is 2026/13/09."},
		"mixed separators":          {dateLine: "Today\u2019s date is 2026/10-09."},
		"unknown apostrophe U+2018": {dateLine: "Today\u2018s date is 2026-10-09."},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			body := f.build(t)
			report := Scan(body)
			for _, line := range report.DateLines {
				if line.Quote.Marked() || line.Slash {
					t.Fatalf("out-of-scope marker detected: %+v", line)
				}
			}
			if got := cleanAll(t, body, report); !bytes.Equal(got, body) {
				t.Fatal("out-of-scope text changed")
			}
		})
	}
}

func TestScanStrayModifiers(t *testing.T) {
	body := fixture{
		dateLine: "Today\u02BCs date is 2026-10-09.",
		userText: "The Hawai\u02BBi word uses \u02BC and \u02B9 and \u02BC again.",
	}.build(t)
	report := Scan(body)
	if report.StrayLab != 2 || report.StrayBoth != 1 {
		t.Fatalf("stray = %d/%d, want 2/1", report.StrayLab, report.StrayBoth)
	}
	cleaned := cleanAll(t, body, report)
	if bytes.Count(cleaned, []byte("\u02BC")) != 2 || bytes.Count(cleaned, []byte("\u02B9")) != 1 {
		t.Fatal("stray modifier letters must not be edited")
	}

	escaped := bytes.ReplaceAll(fixture{dateLine: "Today's date is 2026-10-09.", userText: "a \u02BC b"}.build(t), []byte("\u02BC"), []byte(`\u02BC`))
	escaped = bytes.Replace(escaped, []byte("Please"), []byte(`literal \\u02b9 text`), 1)
	report = Scan(escaped)
	if report.StrayLab != 1 || report.StrayBoth != 0 {
		t.Fatalf("escaped stray = %d/%d, want 1/0", report.StrayLab, report.StrayBoth)
	}
}

func TestScanNonMessagesShapes(t *testing.T) {
	reminder := reminderHead + "Today\u2019s date is 2026/10/09." + reminderTail
	bodies := map[string]any{
		"openai developer role": map[string]any{"model": "gpt-5.5", "messages": []any{
			map[string]any{"role": "developer", "content": "x"},
			map[string]any{"role": "user", "content": reminder},
		}},
		"openai tool role": map[string]any{"model": "gpt-5.5", "messages": []any{
			map[string]any{"role": "user", "content": reminder},
			map[string]any{"role": "tool", "content": "x"},
		}},
		"gemini":    map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": reminder}}}}, "messages": []any{}},
		"responses": map[string]any{"input": reminder, "messages": []any{map[string]any{"role": "user", "content": reminder}}},
	}
	for name, value := range bodies {
		raw, _ := json.Marshal(value)
		if report := Scan(raw); report.Messages || len(report.DateLines) != 0 {
			t.Fatalf("%s: treated as Anthropic Messages", name)
		}
	}
	for _, raw := range []string{``, `[]`, `{"messages":"x"}`, `{"messages":[`, `not json`} {
		if report := Scan([]byte(raw)); report.Messages {
			t.Fatalf("%q treated as Anthropic Messages", raw)
		}
	}
}

func TestScanIdempotent(t *testing.T) {
	body := fixture{dateLine: "Today\u02B9s date is 2026/10/09.", userText: "x \u02BC y"}.build(t)
	first := cleanAll(t, body, Scan(body))
	second := cleanAll(t, first, Scan(first))
	if !bytes.Equal(first, second) {
		t.Fatal("second pass changed the body")
	}
	if c := Scan(first).Counts(); c.QuoteMarkers() != 0 || c.RegionSlash != 0 || c.StrayLab != 1 {
		t.Fatalf("after cleaning: %+v", c)
	}
	// Determinism: repeated scans of the same input agree.
	a, b := Scan(body), Scan(body)
	if len(a.DateLines) != len(b.DateLines) || a.DateLines[0] != b.DateLines[0] {
		t.Fatal("scan is not deterministic")
	}
}

func TestScanMultipleReminders(t *testing.T) {
	body := fixture{dateLine: "Today\u2019s date is 2026/10/09." + reminderTail + reminderHead + "Today\u02BCs date is 2026-10-09."}.build(t)
	report := Scan(body)
	c := report.Counts()
	if c.DateLines != 2 || c.QuoteDomain != 1 || c.QuoteLab != 1 || c.RegionSlash != 1 {
		t.Fatalf("counts = %+v", c)
	}
	if cleaned := cleanAll(t, body, report); strings.Count(string(cleaned), "Today's date is 2026-10-09.") != 2 {
		t.Fatalf("cleaned = %s", cleaned)
	}
}

func TestDecodeLiteralOffsets(t *testing.T) {
	raw := []byte(`"a\n\u00e9\ud83d\ude00é\/"`)
	runes, offsets, ok := decodeLiteral(raw)
	if !ok {
		t.Fatal("decode failed")
	}
	if string(runes) != "a\n\u00e9\U0001F600\u00e9/" {
		t.Fatalf("runes = %q", string(runes))
	}
	want := []int{1, 2, 4, 10, 22, 24, 26}
	if len(offsets) != len(want) {
		t.Fatalf("offsets = %v", offsets)
	}
	for i := range want {
		if offsets[i] != want[i] {
			t.Fatalf("offsets = %v, want %v", offsets, want)
		}
	}
	for _, bad := range []string{`"`, `"\x"`, `"\u12"`, `abc`} {
		if _, _, ok := decodeLiteral([]byte(bad)); ok {
			t.Fatalf("%q decoded", bad)
		}
	}
}
