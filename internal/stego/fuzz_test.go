package stego

import (
	"bytes"
	"encoding/json"
	"testing"
)

// FuzzScan checks that cleaning never breaks a valid JSON body, never touches
// bytes outside the reported spans, and leaves nothing to clean on a rescan.
func FuzzScan(f *testing.F) {
	t := &testing.T{}
	for _, line := range []string{
		"Today's date is 2026-10-09.",
		"Today’s date is 2026/10/09.",
		"Todayʼs date is 2026-01-31.",
		"Todayʹs date is 2026/12/01.",
	} {
		f.Add(fixture{dateLine: line}.build(t))
		f.Add(fixture{dateLine: line, stringForm: true, escapeHTML: true}.build(t))
	}
	f.Add([]byte(`{"messages":[{"role":"user","content":"<system-reminder>\nToday’s date is 2026\/10\/09.\n</system-reminder>"}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		report := Scan(body)
		cleaned := cleanAll(t, body, report)
		if json.Valid(body) && !json.Valid(cleaned) {
			t.Fatalf("cleaning broke JSON:\n%q\n%q", body, cleaned)
		}
		again := Scan(cleaned)
		for _, line := range again.DateLines {
			if line.Quote.Marked() || line.Slash {
				t.Fatalf("marker left after cleaning: %q", cleaned)
			}
		}
		if len(report.DateLines) == 0 && !bytes.Equal(body, cleaned) {
			t.Fatal("body changed without date lines")
		}
	})
}

// FuzzAlignCCVersion checks that cc_version edits are ordered, never overlap
// date line edits, keep a valid JSON body valid, and are idempotent.
func FuzzAlignCCVersion(f *testing.F) {
	for _, billing := range []string{
		"x-anthropic-billing-header: cc_version=2.1.330." + Fingerprint("Fix it.", "2.1.330") + "; cc_entrypoint=cli;",
		"x-anthropic-billing-header: cc_version=2.1.330.000; cc_entrypoint=cli;",
		"x-anthropic-billing-header: cc_entrypoint=cli;",
	} {
		f.Add([]byte(billingBody(billing, "Fix it.")))
	}
	f.Add([]byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=1.2.3.abc;"},{"type":"text","text":"x-anthropic-billing-header:"}],"messages":[{"role":"user","content":"<system-reminder>\nToday’s date is 2026\/10\/09.\n</system-reminder>"}],"metadata":{"a":1,"user_id":"u"}}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		report := ScanWith(body, Options{Billing: true})
		result := AlignCCVersion(report.Billing, "2.1.321")
		var spans []Span
		var repls []string
		for i, e := range result.Edits {
			if i > 0 && result.Edits[i-1].End > e.Start {
				t.Fatalf("overlapping cc edits %+v", result.Edits)
			}
			spans = append(spans, e.Span)
			repls = append(repls, e.Replacement)
		}
		for _, line := range report.DateLines {
			spans = append(spans, line.QuoteSpan)
			repls = append(repls, string(line.Quote.Rune()))
		}
		out := apply(t, body, spans, repls) // fails on overlap
		if !json.Valid(body) {
			return
		}
		if !json.Valid(out) {
			t.Fatalf("edits broke JSON:\n%q\n%q", body, out)
		}
		if again := AlignCCVersion(ScanWith(out, Options{Billing: true}).Billing, "2.1.321"); again.Kind != CCNone {
			t.Fatalf("not idempotent: %+v\n%q", again, out)
		}
	})
}
