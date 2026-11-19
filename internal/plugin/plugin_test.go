package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/sketchain/cpa-stego-guard/internal/stego"
)

// Marker characters, kept as escapes so the source stays readable.
const (
	qDomain = "\u2019"
	qLab    = "\u02BC"
	qBoth   = "\u02B9"
)

func ccBody(t *testing.T, dateLine, userText string) []byte {
	t.Helper()
	reminder := "<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# currentDate\n" +
		dateLine + "\n\n      IMPORTANT: this context may or may not be relevant to your tasks.\n</system-reminder>\n"
	body := map[string]any{
		"model":      "claude-sonnet-5-5",
		"max_tokens": 1024,
		"system": []any{
			map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.150.a1b; cc_entrypoint=cli;"},
			map[string]any{"type": "text", "text": "You are Claude Code. If it" + qDomain + "s unclear, ask."},
		},
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": reminder},
			map[string]any{"type": "text", "text": userText},
		}}},
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

// splice applies edits like CLIProxyAPI's applyRequestBodyEdits.
func splice(t *testing.T, body []byte, edits []bodyEdit) []byte {
	t.Helper()
	sorted := append([]bodyEdit(nil), edits...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })
	var out []byte
	last := 0
	for _, e := range sorted {
		if e.Start < last || e.End > len(body) || e.End < e.Start {
			t.Fatalf("invalid edit %+v", e)
		}
		out = append(out, body[last:e.Start]...)
		out = append(out, e.Replacement...)
		last = e.End
	}
	return append(out, body[last:]...)
}

func TestParseConfig(t *testing.T) {
	cfg, warnings, err := ParseConfig([]byte("enabled: true\npriority: 0\n"))
	if err != nil || len(warnings) != 0 || cfg != DefaultConfig() {
		t.Fatalf("defaults: %+v %v %v", cfg, warnings, err)
	}
	cfg, _, err = ParseConfig([]byte("mode: Block\nclean_quote_markers: false\nclean_region_marker: false\n"))
	if err != nil || cfg != (Config{Mode: ModeBlock}) {
		t.Fatalf("explicit: %+v %v", cfg, err)
	}
	cfg, warnings, _ = ParseConfig([]byte("mode: strict\n"))
	if cfg.Mode != ModeClean || len(warnings) != 1 {
		t.Fatalf("unknown mode: %+v %v", cfg, warnings)
	}
	if _, _, err = ParseConfig([]byte("mode: [")); err == nil {
		t.Fatal("invalid YAML accepted")
	}
}

// TestDecideMatrix covers every mode and switch combination for a body that
// carries both a quote marker and the region marker.
func TestDecideMatrix(t *testing.T) {
	body := ccBody(t, "Today"+qBoth+"s date is 2026/10/09.", "hi")
	report := stego.Scan(body)
	type want struct {
		action Action
		edits  int
	}
	cases := []struct {
		mode          Mode
		quote, region bool
		want          want
	}{
		{ModeClean, true, true, want{ActionCleaned, 3}},
		{ModeClean, true, false, want{ActionCleaned, 1}},
		{ModeClean, false, true, want{ActionCleaned, 2}},
		{ModeClean, false, false, want{ActionLogged, 0}},
		{ModeBlock, true, true, want{ActionBlocked, 0}},
		{ModeBlock, true, false, want{ActionBlocked, 0}},
		{ModeBlock, false, true, want{ActionBlocked, 0}},
		{ModeBlock, false, false, want{ActionLogged, 0}},
		{ModeLog, true, true, want{ActionLogged, 0}},
		{ModeLog, false, false, want{ActionLogged, 0}},
		{ModeOff, true, true, want{ActionNone, 0}},
		{ModeOff, false, false, want{ActionNone, 0}},
	}
	for _, tc := range cases {
		name := fmt.Sprintf("%s/quote=%v/region=%v", tc.mode, tc.quote, tc.region)
		t.Run(name, func(t *testing.T) {
			d := Decide(Config{Mode: tc.mode, CleanQuoteMarkers: tc.quote, CleanRegionMarker: tc.region}, report)
			if d.Action != tc.want.action || len(d.Edits) != tc.want.edits {
				t.Fatalf("got %s with %d edits, want %s with %d", d.Action, len(d.Edits), tc.want.action, tc.want.edits)
			}
			if tc.mode == ModeClean {
				out := string(splice(t, body, d.Edits))
				hasQuote := strings.Contains(out, "Today"+qBoth+"s")
				hasSlash := strings.Contains(out, "2026/10/09")
				if hasQuote == tc.quote || hasSlash == tc.region {
					t.Fatalf("quote kept=%v slash kept=%v", hasQuote, hasSlash)
				}
			}
			if tc.mode == ModeBlock && d.Action == ActionBlocked {
				joined := strings.Join(d.Reasons, "|")
				if strings.Contains(joined, "U+02B9") != tc.quote || strings.Contains(joined, "YYYY/MM/DD") != tc.region {
					t.Fatalf("reasons = %v", d.Reasons)
				}
			}
		})
	}
}

func TestDecideBlockOnlyDisabledCategory(t *testing.T) {
	// Only the region marker is present and its switch is off: forward unchanged.
	report := stego.Scan(ccBody(t, "Today's date is 2026/10/09.", "hi"))
	d := Decide(Config{Mode: ModeBlock, CleanQuoteMarkers: true, CleanRegionMarker: false}, report)
	if d.Action != ActionLogged || len(d.Edits) != 0 {
		t.Fatalf("got %+v", d)
	}
}

func TestDecideCleanBodyUntouched(t *testing.T) {
	body := ccBody(t, "Today's date is 2026-10-09.", "it"+qDomain+"s fine")
	for _, mode := range []Mode{ModeClean, ModeBlock, ModeLog, ModeOff} {
		d := Decide(Config{Mode: mode, CleanQuoteMarkers: true, CleanRegionMarker: true}, stego.Scan(body))
		if d.Action != ActionNone || len(d.Edits) != 0 {
			t.Fatalf("%s: %+v", mode, d)
		}
	}
}

func TestDecideStrayOnlyLogs(t *testing.T) {
	body := ccBody(t, "Today's date is 2026-10-09.", "x "+qLab+" y "+qBoth)
	d := Decide(DefaultConfig(), stego.Scan(body))
	if d.Action != ActionLogged || len(d.Edits) != 0 || d.Counts.StrayLab != 1 || d.Counts.StrayBoth != 1 {
		t.Fatalf("got %+v", d)
	}
}

type recordedLog struct {
	level, message string
}

type fakeHost struct {
	mu   sync.Mutex
	logs []recordedLog
}

func (h *fakeHost) call(method string, payload any) (json.RawMessage, error) {
	if method != methodHostLog {
		return nil, fmt.Errorf("unexpected host method %s", method)
	}
	req := payload.(hostLogRequest)
	h.mu.Lock()
	h.logs = append(h.logs, recordedLog{req.Level, req.Message})
	h.mu.Unlock()
	return json.RawMessage(`{}`), nil
}

func newTestApp(t *testing.T, yaml string) (*App, *fakeHost, registration) {
	t.Helper()
	app := NewApp()
	host := &fakeHost{}
	app.SetHostCaller(host.call)
	req, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte(yaml), SchemaVersion: 10})
	raw, err := app.HandleMethod(methodRegister, req)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err = json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("register: %s %v", raw, err)
	}
	var reg registration
	if err = json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	return app, host, reg
}

func result[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("envelope: %s %v", raw, err)
	}
	var out T
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRegisterCapabilitiesPerMode(t *testing.T) {
	for _, mode := range []Mode{ModeClean, ModeLog, ModeOff} {
		_, _, reg := newTestApp(t, "mode: "+string(mode))
		if !reg.Capabilities.RequestInterceptorPatchBodyBefore || reg.Capabilities.RequestInterceptor {
			t.Fatalf("%s: capabilities %+v", mode, reg.Capabilities)
		}
		if reg.SchemaVersion != 8 || reg.Metadata.Version != Version || reg.Metadata.Name == "" || reg.Metadata.Author == "" || reg.Metadata.GitHubRepository == "" {
			t.Fatalf("registration %+v", reg)
		}
	}
	_, _, reg := newTestApp(t, "mode: block")
	if !reg.Capabilities.RequestInterceptor || !reg.Capabilities.RequestInterceptorOmitBodyAfter || reg.Capabilities.RequestInterceptorPatchBodyBefore {
		t.Fatalf("block capabilities %+v", reg.Capabilities)
	}
	if len(reg.Metadata.ConfigFields) != 3 {
		t.Fatalf("config fields %+v", reg.Metadata.ConfigFields)
	}
}

func TestReconfigureSwitchesMode(t *testing.T) {
	app, _, _ := newTestApp(t, "mode: clean")
	req, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte("mode: block\nclean_region_marker: false\n")})
	raw, err := app.HandleMethod(methodReconfigure, req)
	if err != nil {
		t.Fatal(err)
	}
	reg := result[registration](t, raw)
	if !reg.Capabilities.RequestInterceptor || app.Config() != (Config{Mode: ModeBlock, CleanQuoteMarkers: true}) {
		t.Fatalf("after reconfigure: %+v %+v", reg.Capabilities, app.Config())
	}
}

func TestPatchBodyClean(t *testing.T) {
	app, host, _ := newTestApp(t, "mode: clean")
	host.logs = nil
	body := ccBody(t, "Today"+qDomain+"s date is 2026/10/09.", "secret prompt text")
	resp := result[patchResponse](t, mustHandle(t, app, methodPatchBodyBefore, body))
	out := splice(t, body, resp.Edits)
	if want := ccBody(t, "Today's date is 2026-10-09.", "secret prompt text"); !bytes.Equal(out, want) {
		t.Fatalf("patched body:\n%s\nwant:\n%s", out, want)
	}
	// Idempotent: the patched body needs no further edits.
	if again := result[patchResponse](t, mustHandle(t, app, methodPatchBodyBefore, out)); len(again.Edits) != 0 {
		t.Fatalf("second pass edits: %+v", again.Edits)
	}
	if len(host.logs) != 1 {
		t.Fatalf("logs = %+v", host.logs)
	}
	entry := host.logs[0]
	if !strings.Contains(entry.message, "action=cleaned mode=clean hook=request.patch_body_before date_lines=1 quote_u2019_domain=1 quote_u02bc_lab=0 quote_u02b9_both=0 region_slash_date=1") {
		t.Fatalf("log message = %q", entry.message)
	}
	assertNoPromptText(t, host.logs, "secret prompt text", "Today", "2026")
}

func TestPatchBodyCleanRequestUntouched(t *testing.T) {
	app, host, _ := newTestApp(t, "")
	host.logs = nil
	body := ccBody(t, "Today's date is 2026-10-09.", "hello")
	raw := mustHandle(t, app, methodPatchBodyBefore, body)
	if resp := result[patchResponse](t, raw); len(resp.Edits) != 0 {
		t.Fatalf("edits on clean body: %+v", resp.Edits)
	}
	if !strings.Contains(string(raw), `"result":{}`) {
		t.Fatalf("clean response = %s", raw)
	}
	if len(host.logs) != 0 {
		t.Fatalf("clean body logged: %+v", host.logs)
	}
}

func TestPatchBodyLogAndOff(t *testing.T) {
	body := ccBody(t, "Today"+qLab+"s date is 2026-10-09.", "hello")
	app, host, _ := newTestApp(t, "mode: log")
	host.logs = nil
	if resp := result[patchResponse](t, mustHandle(t, app, methodPatchBodyBefore, body)); len(resp.Edits) != 0 {
		t.Fatal("log mode edited the body")
	}
	if len(host.logs) != 1 || !strings.Contains(host.logs[0].message, "action=logged") || !strings.Contains(host.logs[0].message, "quote_u02bc_lab=1") {
		t.Fatalf("log mode logs = %+v", host.logs)
	}
	app, host, _ = newTestApp(t, "mode: off")
	host.logs = nil
	if resp := result[patchResponse](t, mustHandle(t, app, methodPatchBodyBefore, body)); len(resp.Edits) != 0 {
		t.Fatal("off mode edited the body")
	}
	if len(host.logs) != 0 {
		t.Fatal("off mode logged")
	}
}

func TestPatchBodySwitchesOff(t *testing.T) {
	body := ccBody(t, "Today"+qDomain+"s date is 2026/10/09.", "hello")
	app, host, _ := newTestApp(t, "clean_quote_markers: false")
	host.logs = nil
	out := splice(t, body, result[patchResponse](t, mustHandle(t, app, methodPatchBodyBefore, body)).Edits)
	if !bytes.Contains(out, []byte("Today"+qDomain+"s date is 2026-10-09.")) {
		t.Fatalf("quote marker should stay and slash should go: %s", out)
	}
	if !strings.Contains(host.logs[0].message, "quote_u2019_domain=1") || !strings.Contains(host.logs[0].message, "edits=2") {
		t.Fatalf("log message = %q", host.logs[0].message)
	}
}

func TestInterceptBlock(t *testing.T) {
	app, host, _ := newTestApp(t, "mode: block")
	host.logs = nil
	body := ccBody(t, "Today"+qDomain+"s date is 2026-10-09.", "hello")
	req, _ := json.Marshal(interceptRequest{RequestID: "req-1", SourceFormat: "claude", Body: body})
	resp := result[interceptResponse](t, mustHandle(t, app, methodInterceptBefore, req))
	if !resp.Terminate || resp.StatusCode != 400 || len(resp.BodyEdits) != 0 {
		t.Fatalf("block response = %+v", resp)
	}
	if got := resp.ResponseHeaders["Content-Type"]; len(got) != 1 || got[0] != "application/json" {
		t.Fatalf("headers = %+v", resp.ResponseHeaders)
	}
	var errBody struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.ResponseBody, &errBody); err != nil {
		t.Fatal(err)
	}
	if errBody.Type != "error" || errBody.Error.Type != "invalid_request_error" ||
		!strings.Contains(errBody.Error.Message, "U+2019") || !strings.Contains(errBody.Error.Message, "2.1.293") {
		t.Fatalf("error body = %s", resp.ResponseBody)
	}
	if len(host.logs) != 1 || host.logs[0].level != "warn" || !strings.Contains(host.logs[0].message, "action=blocked") || !strings.Contains(host.logs[0].message, "plugin_request_id=req-1") {
		t.Fatalf("logs = %+v", host.logs)
	}
	// The after phase is a no-op.
	if raw := mustHandle(t, app, methodInterceptAfter, req); !strings.Contains(string(raw), `"ok":true`) {
		t.Fatalf("after = %s", raw)
	}
}

func TestInterceptPassThrough(t *testing.T) {
	app, _, _ := newTestApp(t, "mode: block")
	marked := ccBody(t, "Today"+qDomain+"s date is 2026-10-09.", "hello")
	clean := ccBody(t, "Today's date is 2026-10-09.", "hello")
	cases := map[string]interceptRequest{
		"clean claude body":    {SourceFormat: "claude", Body: clean},
		"other source format":  {SourceFormat: "openai", Body: marked},
		"empty body":           {SourceFormat: "claude"},
		"region switch is off": {SourceFormat: "claude", Body: ccBody(t, "Today's date is 2026/10/09.", "x")},
	}
	app.config.Store(&Config{Mode: ModeBlock, CleanQuoteMarkers: true, CleanRegionMarker: false})
	for name, req := range cases {
		raw, _ := json.Marshal(req)
		resp := result[interceptResponse](t, mustHandle(t, app, methodInterceptBefore, raw))
		if resp.Terminate || len(resp.BodyEdits) != 0 {
			t.Fatalf("%s: %+v", name, resp)
		}
	}
}

func TestUnknownMethod(t *testing.T) {
	app := NewApp()
	raw, err := app.HandleMethod("executor.execute", nil)
	if err != nil || !strings.Contains(string(raw), "unknown_method") {
		t.Fatalf("%s %v", raw, err)
	}
	for _, method := range []string{methodQuiesce, methodShutdown} {
		if raw, _ := app.HandleMethod(method, nil); !strings.Contains(string(raw), `"ok":true`) {
			t.Fatalf("%s: %s", method, raw)
		}
	}
}

func TestConcurrentPatchAndReconfigure(t *testing.T) {
	app, _, _ := newTestApp(t, "")
	body := ccBody(t, "Today"+qBoth+"s date is 2026/10/09.", "hello")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if i == 0 {
					req, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte("mode: clean")})
					_, _ = app.HandleMethod(methodReconfigure, req)
					continue
				}
				resp := result[patchResponse](t, mustHandle(t, app, methodPatchBodyBefore, body))
				if len(resp.Edits) != 3 {
					t.Errorf("edits = %d", len(resp.Edits))
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

func mustHandle(t *testing.T, app *App, method string, request []byte) []byte {
	t.Helper()
	raw, err := app.HandleMethod(method, request)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return raw
}

func assertNoPromptText(t *testing.T, logs []recordedLog, needles ...string) {
	t.Helper()
	for _, entry := range logs {
		for _, needle := range needles {
			if strings.Contains(entry.message, needle) {
				t.Fatalf("log leaks %q: %s", needle, entry.message)
			}
		}
	}
}
