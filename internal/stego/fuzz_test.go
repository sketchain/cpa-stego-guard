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
