package plugin

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sketchain/cpa-stego-guard/internal/stego"
)

const (
	clientVersion = "2.1.330"
	targetVersion = "2.1.321"
	userPrompt    = "Fix the failing test."
)

// ccRequest builds a Claude Code request whose billing header carries
// version and fingerprint fp. stringContent puts the reminder and the prompt
// into one string, as some clients send the first user message.
func ccRequest(t *testing.T, dateLine, version, fp string, stringContent bool) []byte {
	t.Helper()
	reminder := "<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# currentDate\n" +
		dateLine + "\n</system-reminder>\n"
	var content any = []any{
		map[string]any{"type": "text", "text": reminder},
		map[string]any{"type": "text", "text": userPrompt},
	}
	if stringContent {
		content = reminder + userPrompt
	}
	body := map[string]any{
		"model": "claude-sonnet-5-5",
		"system": []any{
			map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=" + version + "." + fp + "; cc_entrypoint=cli;"},
			map[string]any{"type": "text", "text": "You are Claude Code, Anthropic" + qDomain + "s official CLI for Claude."},
		},
		"messages": []any{map[string]any{"role": "user", "content": content}},
		"metadata": map[string]any{"user_id": `{"device_id":"d","account_uuid":"a","session_id":"s"}`},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		t.Fatal(err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// fingerprintSource is the text Claude Code hashes for ccRequest.
func fingerprintSource(dateLine string, stringContent bool) string {
	if !stringContent {
		return userPrompt
	}
	return "<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# currentDate\n" +
		dateLine + "\n</system-reminder>\n" + userPrompt
}

type upstreamView struct {
	System []struct {
		Text string `json:"text"`
	} `json:"system"`
	Messages []struct {
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Metadata map[string]any `json:"metadata"`
}

func view(t *testing.T, body []byte) upstreamView {
	t.Helper()
	var v upstreamView
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, body)
	}
	return v
}

func TestParseConfigCCVersion(t *testing.T) {
	cfg, warnings, err := ParseConfig([]byte("cc_version: ' 2.1.321 '\n"))
	if err != nil || len(warnings) != 0 || cfg.CCVersion != "2.1.321" {
		t.Fatalf("valid: %+v %v %v", cfg, warnings, err)
	}
	cfg, warnings, _ = ParseConfig([]byte("cc_version: claude-cli/2.1.321\n"))
	if cfg.CCVersion != "" || len(warnings) != 1 {
		t.Fatalf("invalid: %+v %v", cfg, warnings)
	}
	cfg, warnings, _ = ParseConfig([]byte("cc_version: ''\n"))
	if cfg.CCVersion != "" || len(warnings) != 0 {
		t.Fatalf("empty: %+v %v", cfg, warnings)
	}
}

func TestRegisterWarnsWithoutCCVersion(t *testing.T) {
	_, host, _ := newTestApp(t, "mode: clean")
	if len(host.logs) != 2 || host.logs[0].level != "warn" || !strings.Contains(host.logs[0].message, "cc_version is empty") ||
		!strings.Contains(host.logs[1].message, "cc_version=(disabled)") {
		t.Fatalf("logs = %+v", host.logs)
	}
	_, host, _ = newTestApp(t, "cc_version: "+targetVersion)
	if len(host.logs) != 1 || !strings.Contains(host.logs[0].message, "cc_version="+targetVersion) {
		t.Fatalf("logs = %+v", host.logs)
	}
	_, host, _ = newTestApp(t, "mode: off")
	if len(host.logs) != 1 {
		t.Fatalf("off mode should not warn: %+v", host.logs)
	}
}

func TestPatchBodyAlignsCCVersion(t *testing.T) {
	app, host, _ := newTestApp(t, "cc_version: "+targetVersion)
	host.logs = nil
	date := "Today's date is 2026-10-09."
	body := ccRequest(t, date, clientVersion, stego.Fingerprint(userPrompt, clientVersion), false)
	resp := result[patchResponse](t, mustHandle(t, app, methodPatchBodyBefore, body))
	out := splice(t, body, resp.Edits)
	want := ccRequest(t, date, targetVersion, stego.Fingerprint(userPrompt, targetVersion), false)
	if !bytes.Equal(out, want) {
		t.Fatalf("patched:\n%s\nwant:\n%s", out, want)
	}
	if again := result[patchResponse](t, mustHandle(t, app, methodPatchBodyBefore, out)); len(again.Edits) != 0 {
		t.Fatalf("second pass edits: %+v", again.Edits)
	}
	if len(host.logs) != 1 || host.logs[0].level != "debug" ||
		!strings.Contains(host.logs[0].message, "action=aligned mode=clean hook=request.patch_body_before cc_version=align cc_version_applied=true cc_version_from=2.1.330 cc_version_to=2.1.321") {
		t.Fatalf("logs = %+v", host.logs)
	}
	assertNoPromptText(t, host.logs, userPrompt)
}

func TestPatchBodyCCVersionUnchanged(t *testing.T) {
	date := "Today's date is 2026-10-09."
	// Already at the target version: untouched and not logged.
	app, host, _ := newTestApp(t, "cc_version: "+targetVersion)
	host.logs = nil
	body := ccRequest(t, date, targetVersion, stego.Fingerprint(userPrompt, targetVersion), false)
	if resp := result[patchResponse](t, mustHandle(t, app, methodPatchBodyBefore, body)); len(resp.Edits) != 0 || len(host.logs) != 0 {
		t.Fatalf("same version: %+v %+v", resp, host.logs)
	}
	// Already at the target version but with a tampered fingerprint: fallback.
	host.logs = nil
	bad := "000"
	if bad == stego.Fingerprint(userPrompt, targetVersion) {
		bad = "001"
	}
	body = ccRequest(t, date, targetVersion, bad, false)
	out := splice(t, body, result[patchResponse](t, mustHandle(t, app, methodPatchBodyBefore, body)).Edits)
	if v := view(t, out); len(v.System) != 1 || v.Metadata["user_id"] != nil {
		t.Fatalf("same version, tampered: billing header or user_id kept: %s", out)
	}
	if len(host.logs) != 1 || !strings.Contains(host.logs[0].message, "cc_version=fallback cc_version_applied=true cc_version_from=2.1.321 cc_version_to=2.1.321 cc_version_reason=fingerprint_mismatch") {
		t.Fatalf("same version, tampered: logs = %+v", host.logs)
	}
	// cc_version not configured: an older client and a tampered one both pass.
	app, host, _ = newTestApp(t, "mode: clean")
	host.logs = nil
	for _, fp := range []string{stego.Fingerprint(userPrompt, clientVersion), "zzz"} {
		body = ccRequest(t, date, clientVersion, fp, false)
		if resp := result[patchResponse](t, mustHandle(t, app, methodPatchBodyBefore, body)); len(resp.Edits) != 0 {
			t.Fatalf("disabled alignment edited: %+v", resp)
		}
	}
	if len(host.logs) != 0 {
		t.Fatalf("logs = %+v", host.logs)
	}
}

func TestPatchBodyCCVersionFallback(t *testing.T) {
	app, host, _ := newTestApp(t, "cc_version: "+targetVersion)
	host.logs = nil
	good := stego.Fingerprint(userPrompt, clientVersion)
	bad := "000"
	if bad == good {
		bad = "001"
	}
	body := ccRequest(t, "Today's date is 2026-10-09.", clientVersion, bad, false)
	out := splice(t, body, result[patchResponse](t, mustHandle(t, app, methodPatchBodyBefore, body)).Edits)
	v := view(t, out)
	if len(v.System) != 1 || strings.HasPrefix(v.System[0].Text, stego.BillingPrefix) {
		t.Fatalf("billing header kept: %s", out)
	}
	if _, ok := v.Metadata["user_id"]; ok || v.Metadata == nil {
		t.Fatalf("metadata.user_id kept: %s", out)
	}
	if again := result[patchResponse](t, mustHandle(t, app, methodPatchBodyBefore, out)); len(again.Edits) != 0 {
		t.Fatalf("second pass edits: %+v", again.Edits)
	}
	if len(host.logs) != 1 || host.logs[0].level != "info" ||
		!strings.Contains(host.logs[0].message, "action=fallback mode=clean hook=request.patch_body_before cc_version=fallback cc_version_applied=true cc_version_from=2.1.330 cc_version_to=2.1.321 cc_version_reason=fingerprint_mismatch") {
		t.Fatalf("logs = %+v", host.logs)
	}
}

func TestPatchBodyCCVersionModes(t *testing.T) {
	body := ccRequest(t, "Today's date is 2026-10-09.", clientVersion, stego.Fingerprint(userPrompt, clientVersion), false)
	app, host, _ := newTestApp(t, "mode: log\ncc_version: "+targetVersion)
	host.logs = nil
	if resp := result[patchResponse](t, mustHandle(t, app, methodPatchBodyBefore, body)); len(resp.Edits) != 0 {
		t.Fatal("log mode edited the body")
	}
	if len(host.logs) != 1 || !strings.Contains(host.logs[0].message, "would align cc_version action=logged") ||
		!strings.Contains(host.logs[0].message, "cc_version_applied=false") {
		t.Fatalf("log mode logs = %+v", host.logs)
	}
	app, host, _ = newTestApp(t, "mode: off\ncc_version: "+targetVersion)
	host.logs = nil
	if resp := result[patchResponse](t, mustHandle(t, app, methodPatchBodyBefore, body)); len(resp.Edits) != 0 || len(host.logs) != 0 {
		t.Fatal("off mode acted")
	}
	// A patch call that races a reconfiguration to block mode only logs.
	app, host, _ = newTestApp(t, "mode: block\ncc_version: "+targetVersion)
	host.logs = nil
	if resp := result[patchResponse](t, mustHandle(t, app, methodPatchBodyBefore, body)); len(resp.Edits) != 0 {
		t.Fatal("patch hook in block mode edited the body")
	}
}

func TestInterceptBlockModeCCVersion(t *testing.T) {
	app, host, _ := newTestApp(t, "mode: block\ncc_version: "+targetVersion)
	intercept := func(body []byte) interceptResponse {
		req, _ := json.Marshal(interceptRequest{RequestID: "r", SourceFormat: "claude", Body: body})
		return result[interceptResponse](t, mustHandle(t, app, methodInterceptBefore, req))
	}
	clean := "Today's date is 2026-10-09."
	fp := stego.Fingerprint(userPrompt, clientVersion)

	// Not blocked: aligned like clean mode.
	body := ccRequest(t, clean, clientVersion, fp, false)
	resp := intercept(body)
	want := ccRequest(t, clean, targetVersion, stego.Fingerprint(userPrompt, targetVersion), false)
	if resp.Terminate || !bytes.Equal(splice(t, body, resp.BodyEdits), want) {
		t.Fatalf("block mode alignment: %+v", resp)
	}
	// Tampered fingerprint, not blocked: falls back.
	body = ccRequest(t, clean, clientVersion, "fff", false)
	if v := view(t, splice(t, body, intercept(body).BodyEdits)); len(v.System) != 1 {
		t.Fatalf("block mode fallback: %+v", v)
	}
	// Blocked for a date line marker: rejected, nothing else.
	host.logs = nil
	body = ccRequest(t, "Today"+qDomain+"s date is 2026-10-09.", clientVersion, fp, false)
	resp = intercept(body)
	if !resp.Terminate || len(resp.BodyEdits) != 0 || strings.Contains(host.logs[0].message, "cc_version=") {
		t.Fatalf("blocked: %+v %+v", resp, host.logs)
	}
	// Stray U+02BC only: logged, not blocked, still aligned.
	body = ccRequest(t, clean, clientVersion, fp, false)
	body = bytes.Replace(body, []byte(userPrompt+`"`), []byte(userPrompt+qLab+`"`), 1)
	resp = intercept(body)
	if resp.Terminate || len(resp.BodyEdits) != 1 {
		t.Fatalf("stray only: %+v", resp)
	}
}

// TestCCVersionWithDateCleaning applies both rewrites to one request. The date
// reminder never enters the fingerprint text when it is its own block, and
// when the reminder and the prompt share one string the cleaning replaces one
// UTF-16 unit with one unit past index 20, so the fingerprint is the same
// before and after cleaning.
func TestCCVersionWithDateCleaning(t *testing.T) {
	marked := "Today" + qBoth + "s date is 2026/10/09."
	clean := "Today's date is 2026-10-09."
	for _, stringContent := range []bool{false, true} {
		source := fingerprintSource(marked, stringContent)
		cleanSource := fingerprintSource(clean, stringContent)
		if stego.Fingerprint(source, targetVersion) != stego.Fingerprint(cleanSource, targetVersion) ||
			stego.Fingerprint(source, clientVersion) != stego.Fingerprint(cleanSource, clientVersion) {
			t.Fatalf("string=%v: cleaning changes the fingerprint", stringContent)
		}
		app, host, _ := newTestApp(t, "cc_version: "+targetVersion)
		host.logs = nil
		body := ccRequest(t, marked, clientVersion, stego.Fingerprint(source, clientVersion), stringContent)
		resp := result[patchResponse](t, mustHandle(t, app, methodPatchBodyBefore, body))
		out := splice(t, body, resp.Edits)
		want := ccRequest(t, clean, targetVersion, stego.Fingerprint(cleanSource, targetVersion), stringContent)
		if !bytes.Equal(out, want) {
			t.Fatalf("string=%v patched:\n%s\nwant:\n%s", stringContent, out, want)
		}
		if len(resp.Edits) != 4 {
			t.Fatalf("string=%v: %d edits, want 3 date edits + 1 cc_version edit", stringContent, len(resp.Edits))
		}
		if again := result[patchResponse](t, mustHandle(t, app, methodPatchBodyBefore, out)); len(again.Edits) != 0 {
			t.Fatalf("string=%v second pass edits: %+v", stringContent, again.Edits)
		}
		msg := host.logs[0].message
		if !strings.Contains(msg, "action=cleaned") || !strings.Contains(msg, "cc_version=align cc_version_applied=true") || !strings.Contains(msg, "edits=4") {
			t.Fatalf("log = %q", msg)
		}

		// Cleaning and the fallback together: disjoint edits, valid JSON.
		body = ccRequest(t, marked, clientVersion, "abc", stringContent)
		if stego.Fingerprint(source, clientVersion) == "abc" {
			body = ccRequest(t, marked, clientVersion, "abd", stringContent)
		}
		out = splice(t, body, result[patchResponse](t, mustHandle(t, app, methodPatchBodyBefore, body)).Edits)
		v := view(t, out)
		if len(v.System) != 1 || v.Metadata["user_id"] != nil || !bytes.Contains(out, []byte(clean)) || bytes.Contains(out, []byte(marked)) {
			t.Fatalf("string=%v fallback with cleaning: %s", stringContent, out)
		}
	}
}

// TestDecideCCVersionMatrix pins which modes apply the cc_version edits.
func TestDecideCCVersionMatrix(t *testing.T) {
	body := ccRequest(t, "Today's date is 2026-10-09.", clientVersion, stego.Fingerprint(userPrompt, clientVersion), false)
	report := stego.ScanWith(body, stego.Options{Billing: true})
	for mode, applied := range map[Mode]bool{ModeClean: true, ModeBlock: true, ModeLog: false, ModeOff: false} {
		cfg := DefaultConfig()
		cfg.Mode = mode
		cfg.CCVersion = targetVersion
		d := Decide(cfg, report)
		if d.CCApplied != applied || (len(d.Edits) == 1) != applied {
			t.Fatalf("%s: applied=%v edits=%d", mode, d.CCApplied, len(d.Edits))
		}
		wantKind := stego.CCAlign
		if mode == ModeOff {
			wantKind = stego.CCNone
		}
		if d.CC.Kind != wantKind {
			t.Fatalf("%s: kind %s", mode, d.CC.Kind)
		}
	}
}
