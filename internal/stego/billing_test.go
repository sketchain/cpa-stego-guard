package stego

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestFingerprintKnownValues checks the algorithm against values from
// CLIProxyAPI's own tests (computeFingerprint), which match Claude Code
// captures.
func TestFingerprintKnownValues(t *testing.T) {
	cases := []struct{ text, version, want string }{
		{"CPA_OFFICIAL_BASEURL_CLI_SYSTEM_EMPTY_b82d4e", "2.1.258", "1f4"},
		{"say ok", "2.1.321", "20d"},
		{"😀abcdefghijklmnopqrstuvwxyz", "2.1.280", "dac"}, // emoji shifts the UTF-16 indexes
		{"abc😀abcdefghijklmnop", "2.1.280", "695"},        // index 4 samples a lone low surrogate
	}
	for _, tc := range cases {
		if got := Fingerprint(tc.text, tc.version); got != tc.want {
			t.Errorf("Fingerprint(%q, %s) = %s, want %s", tc.text, tc.version, got, tc.want)
		}
	}
}

// TestFingerprintTextAndValueMatchCLIProxyAPI compares the text selection and
// fingerprint with the output of CLIProxyAPI's claudeBillingFingerprintMessageText
// and computeFingerprint for the same request bodies.
func TestFingerprintTextAndValueMatchCLIProxyAPI(t *testing.T) {
	cases := []struct{ payload, version, want string }{
		{`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# currentDate\nToday's date is 2026-10-09.\n</system-reminder>\n"},{"type":"text","text":"<system-reminder>\nctx\n</system-reminder>"},{"type":"text","text":"bcZZch"}]}]}`, "2.1.321", "3e6"},
		{`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# currentDate\nToday's date is 2026-10-09.\n</system-reminder>\n\\gXd3\tc'b\"g1\n3Z\ud83d\ude00 0"}]}]}`, "2.1.9", "e32"},
		{`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# currentDate\nToday's date is 2026-10-09.\n</system-reminder>\n"},{"type":"text","text":"<system-reminder>\nctx\n</system-reminder>"},{"type":"text","text":"\u00e90c\u4e2dci1\t\ncb/\tj\\'\n\u4e2d"}]}]}`, "2.1.9", "dc9"},
		{`{"model":"m","messages":[{"role":"assistant","content":"x"},{"role":"user","content":[{"type":"image","source":{}},{"type":"text","text":"b\ud83d\ude00d203a\ud83d\ude00"},{"type":"text","text":"later"}]}]}`, "2.1.321", "d2e"},
		{`{"model":"m","messages":[{"role":"user","content":"ab\ud800cdefgh"}]}`, "2.1.321", "3e5"},
		{`{"model":"m","messages":[{"role":"user","content":"abcd\udc00efghij"}]}`, "2.1.321", "c33"},
		{`{"model":"m","messages":[{"role":"user","content":"abc\ud83d\ude00defghijklmnopqrstuvw"}]}`, "2.1.321", "7f3"},
	}
	for i, tc := range cases {
		doc, ok := parseMessages([]byte(tc.payload))
		if !ok {
			t.Fatalf("case %d: not a Messages body", i)
		}
		text, known := fingerprintText([]byte(tc.payload), doc.firstUser)
		if !known {
			t.Fatalf("case %d: fingerprint text unknown", i)
		}
		if got := fingerprintRunes(text, tc.version); got != tc.want {
			t.Errorf("case %d: fingerprint %s, want %s (text %q)", i, got, tc.want, string(text))
		}
	}
}

func TestFingerprintTextSelection(t *testing.T) {
	const date = `<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# currentDate\nToday's date is 2026-10-09.\n</system-reminder>\n`
	cases := []struct {
		name, messages, want string
		known                bool
	}{
		{"string content", `[{"role":"user","content":"say ok"}]`, "say ok", true},
		{"no user message", `[{"role":"assistant","content":"x"}]`, "", true},
		{"first user only", `[{"role":"user","content":"one"},{"role":"user","content":"two"}]`, "one", true},
		{"skips reminders", `[{"role":"user","content":[{"type":"text","text":"` + date + `"},{"type":"text","text":"<system-reminder>ctx</system-reminder>"},{"type":"text","text":"real"},{"type":"text","text":"more"}]}]`, "real", true},
		{"date reminder without close tag", `[{"role":"user","content":[{"type":"text","text":"<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# currentDate\nToday's date is 2026-10-09."},{"type":"text","text":"real"}]}]`, "real", true},
		{"open reminder only is text", `[{"role":"user","content":[{"type":"text","text":"<system-reminder> open"},{"type":"text","text":"real"}]}]`, "<system-reminder> open", true},
		{"only reminders", `[{"role":"user","content":[{"type":"text","text":"<system-reminder>a</system-reminder>"}]}]`, "", true},
		{"non-text blocks skipped", `[{"role":"user","content":[{"type":"image","source":{}},{"type":"tool_result","content":"x"},{"type":"text","text":"real"}]}]`, "real", true},
		{"missing text ends search", `[{"role":"user","content":[{"type":"text"},{"type":"text","text":"real"}]}]`, "", true},
		{"escaped text", `[{"role":"user","content":"say \"ok\""}]`, `say "ok"`, true},
		{"numeric text is unknown", `[{"role":"user","content":[{"type":"text","text":5}]}]`, "", false},
		{"object content", `[{"role":"user","content":{"a":1}}]`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"m","messages":` + tc.messages + `}`)
			doc, ok := parseMessages(body)
			if !ok {
				t.Fatal("not a Messages body")
			}
			text, known := fingerprintText(body, doc.firstUser)
			if known != tc.known || string(text) != tc.want {
				t.Fatalf("got %q known=%v, want %q known=%v", string(text), known, tc.want, tc.known)
			}
		})
	}
}

func TestValidVersion(t *testing.T) {
	for v, want := range map[string]bool{
		"2.1.321": true, "10.20.300": true, "2.1": false, "2.1.321.1": false, "v2.1.321": false,
		"2.1.x": false, "": false, "2..1": false, "2.1.321 ": false, "２.1.3": false,
	} {
		if got := ValidVersion(v); got != want {
			t.Errorf("ValidVersion(%q) = %v", v, got)
		}
	}
}

func billingBody(billing, userText string) string {
	return `{"model":"m","max_tokens":1,"system":[{"type":"text","text":"` + billing + `"},{"type":"text","text":"You are Claude Code."}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>ctx</system-reminder>"},{"type":"text","text":"` + userText + `"}]}],` +
		`"metadata":{"user_id":"{\"device_id\":\"d\",\"session_id\":\"s\"}"}}`
}

func align(t *testing.T, body, target string) (CCResult, string) {
	t.Helper()
	report := ScanWith([]byte(body), Options{Billing: true})
	result := AlignCCVersion(report.Billing, target)
	out := []byte(body)
	for i := len(result.Edits) - 1; i >= 0; i-- {
		e := result.Edits[i]
		if i > 0 && result.Edits[i-1].End > e.Start {
			t.Fatalf("overlapping edits %+v", result.Edits)
		}
		out = append(out[:e.Start:e.Start], append([]byte(e.Replacement), out[e.End:]...)...)
	}
	if !json.Valid(out) {
		t.Fatalf("invalid JSON after edits: %s", out)
	}
	return result, string(out)
}

func TestAlignCCVersion(t *testing.T) {
	fp := Fingerprint("Fix it.", "2.1.330")
	body := billingBody("x-anthropic-billing-header: cc_version=2.1.330."+fp+"; cc_entrypoint=cli; cch=1a2b3;", "Fix it.")

	result, out := align(t, body, "2.1.321")
	want := "cc_version=2.1.321." + Fingerprint("Fix it.", "2.1.321") + "; cc_entrypoint=cli; cch=1a2b3;"
	if result.Kind != CCAlign || result.From != "2.1.330" || !strings.Contains(out, want) {
		t.Fatalf("align: %+v\n%s", result, out)
	}
	if strings.Replace(out, want, "cc_version=2.1.330."+fp+"; cc_entrypoint=cli; cch=1a2b3;", 1) != body {
		t.Fatal("alignment changed bytes outside cc_version")
	}
	// Idempotent: the aligned body is left alone.
	if again, _ := align(t, out, "2.1.321"); again.Kind != CCNone || len(again.Edits) != 0 {
		t.Fatalf("second pass: %+v", again)
	}
	// Same version as the target, even with a wrong fingerprint: untouched.
	same := billingBody("x-anthropic-billing-header: cc_version=2.1.321.fff; cc_entrypoint=cli;", "Fix it.")
	if r, _ := align(t, same, "2.1.321"); r.Kind != CCNone {
		t.Fatalf("same version: %+v", r)
	}
	// Empty target disables everything, including the fallback.
	if r, _ := align(t, body, ""); r.Kind != CCNone {
		t.Fatalf("empty target: %+v", r)
	}
	tampered := billingBody("x-anthropic-billing-header: cc_version=2.1.330.000; cc_entrypoint=cli;", "Fix it.")
	if r, _ := align(t, tampered, ""); r.Kind != CCNone {
		t.Fatalf("empty target, tampered: %+v", r)
	}
	// No billing header (not Claude Code): nothing to do.
	plain := `{"model":"m","messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"x"}}`
	if r, _ := align(t, plain, "2.1.321"); r.Kind != CCNone {
		t.Fatalf("no billing: %+v", r)
	}
}

func TestAlignCCVersionFallback(t *testing.T) {
	good := Fingerprint("Fix it.", "2.1.330")
	bad := "000"
	if bad == good {
		bad = "001"
	}
	cases := []struct {
		name, billing, reason string
	}{
		{"fingerprint mismatch", "x-anthropic-billing-header: cc_version=2.1.330." + bad + "; cc_entrypoint=cli;", ReasonFingerprintInvalid},
		{"no cc_version", "x-anthropic-billing-header: cc_entrypoint=cli;", ReasonUnrecognized},
		{"short version", "x-anthropic-billing-header: cc_version=2.1." + good + "; cc_entrypoint=cli;", ReasonUnrecognized},
		{"uppercase fingerprint", "x-anthropic-billing-header: cc_version=2.1.330.ABC; cc_entrypoint=cli;", ReasonUnrecognized},
		{"four digit fingerprint", "x-anthropic-billing-header: cc_version=2.1.330." + good + "0; cc_entrypoint=cli;", ReasonUnrecognized},
		{"no terminator", "x-anthropic-billing-header: cc_version=2.1.330." + good, ReasonUnrecognized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, out := align(t, billingBody(tc.billing, "Fix it."), "2.1.321")
			if result.Kind != CCFallback || result.Reason != tc.reason {
				t.Fatalf("got %+v", result)
			}
			if strings.Contains(out, BillingPrefix) || strings.Contains(out, "user_id") {
				t.Fatalf("billing header or user_id left: %s", out)
			}
			want := `"system":[{"type":"text","text":"You are Claude Code."}]`
			if !strings.Contains(out, want) || !strings.Contains(out, `"metadata":{}`) {
				t.Fatalf("unexpected shape: %s", out)
			}
			if again, _ := align(t, out, "2.1.321"); again.Kind != CCNone {
				t.Fatalf("second pass: %+v", again)
			}
		})
	}
	// A numeric first user text cannot be hashed the way the client did.
	body := strings.Replace(billingBody("x-anthropic-billing-header: cc_version=2.1.330."+good+"; cc_entrypoint=cli;", "x"), `"text":"x"`, `"text":7`, 1)
	if r, _ := align(t, body, "2.1.321"); r.Kind != CCFallback || r.Reason != ReasonFingerprintUnknown {
		t.Fatalf("unknown text: %+v", r)
	}
}

// TestAlignCCVersionDeletesKeepJSONValid removes billing blocks and user_id in
// every position of their containers.
func TestAlignCCVersionDeletesKeepJSONValid(t *testing.T) {
	billing := `{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.330.000; cc_entrypoint=cli;"}`
	other := `{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}`
	systems := map[string]string{
		"only":        "[" + billing + "]",
		"first":       "[" + billing + "," + other + "]",
		"middle":      "[ " + other + " , " + billing + " ,\n" + other + " ]",
		"last":        "[" + other + "," + billing + "]",
		"two at end":  "[" + other + "," + billing + "," + billing + "]",
		"two leading": "[" + billing + "," + billing + "," + other + "]",
		"all":         "[" + billing + "," + billing + "]",
	}
	metadatas := map[string]string{
		"user_id only": `{"user_id":"u"}`,
		"user_id last": `{"a":{"b":[1,"}"]},"user_id":"u"}`,
		"user_id mid":  `{ "a" : 1 , "user_id" : "u\"}" , "c" : null }`,
		"none":         `{"a":1}`,
	}
	for sname, system := range systems {
		for mname, metadata := range metadatas {
			body := `{"system":` + system + `,"messages":[{"role":"user","content":"Fix it."}],"metadata":` + metadata + `}`
			result, out := align(t, body, "2.1.321")
			if result.Kind != CCFallback {
				t.Fatalf("%s/%s: %+v", sname, mname, result)
			}
			var doc struct {
				System   []map[string]any `json:"system"`
				Metadata map[string]any   `json:"metadata"`
			}
			if err := json.Unmarshal([]byte(out), &doc); err != nil {
				t.Fatalf("%s/%s: %v\n%s", sname, mname, err, out)
			}
			for _, block := range doc.System {
				if text, _ := block["text"].(string); strings.HasPrefix(text, BillingPrefix) {
					t.Fatalf("%s/%s: billing left: %s", sname, mname, out)
				}
			}
			if _, ok := doc.Metadata["user_id"]; ok {
				t.Fatalf("%s/%s: user_id left: %s", sname, mname, out)
			}
			if wantOthers := strings.Count(system, "You are Claude Code."); len(doc.System) != wantOthers {
				t.Fatalf("%s/%s: %d system blocks left, want %d: %s", sname, mname, len(doc.System), wantOthers, out)
			}
		}
	}
}

// TestAlignCCVersionEscapedBilling handles a billing header written with JSON
// escapes: the edit covers exactly the escaped bytes of the value.
func TestAlignCCVersionEscapedBilling(t *testing.T) {
	fp := Fingerprint("Fix it.", "2.1.330")
	escaped := `x-anthropic-billing-header: cc_version=2.1.330.` + fp + `; cc_entrypoint=cli;`
	result, out := align(t, billingBody(escaped, "Fix it."), "2.1.321")
	if result.Kind != CCAlign || !strings.Contains(out, "cc_version=2.1.321."+Fingerprint("Fix it.", "2.1.321")+";") {
		t.Fatalf("%+v\n%s", result, out)
	}
}

// TestAlignCCVersionSkipsNonMessages leaves other protocols alone.
func TestAlignCCVersionSkipsNonMessages(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"developer","content":"x"}],"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.330.000;"}]}`
	report := ScanWith([]byte(body), Options{Billing: true})
	if report.Messages || AlignCCVersion(report.Billing, "2.1.321").Kind != CCNone {
		t.Fatalf("non-Messages body handled: %+v", report)
	}
	// Scan without the option never reads the billing header.
	if r := Scan([]byte(billingBody("x-anthropic-billing-header: cc_version=2.1.330.000;", "x"))); len(r.Billing.Blocks) != 0 {
		t.Fatal("Scan read the billing header")
	}
}
