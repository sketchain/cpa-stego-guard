package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/sketchain/cpa-stego-guard/internal/stego"
)

// sourceFormatClaude is CLIProxyAPI's identifier of the Anthropic Messages API
// (the /v1/messages handler).
const sourceFormatClaude = "claude"

// HostCaller performs a host callback and returns its result.
type HostCaller func(method string, payload any) (json.RawMessage, error)

// App is the plugin state shared by every RPC call. Calls may arrive
// concurrently; the configuration is swapped atomically on reconfigure. The
// plugin starts no goroutines and no timers.
type App struct {
	config atomic.Pointer[Config]
	hostMu sync.RWMutex
	host   HostCaller
}

// NewApp returns an App with the default configuration.
func NewApp() *App {
	app := &App{}
	cfg := DefaultConfig()
	app.config.Store(&cfg)
	return app
}

// SetHostCaller installs the host callback used for logging.
func (a *App) SetHostCaller(host HostCaller) {
	a.hostMu.Lock()
	a.host = host
	a.hostMu.Unlock()
}

// Config returns the active configuration.
func (a *App) Config() Config {
	return *a.config.Load()
}

// Shutdown releases nothing; the plugin holds no resources.
func (a *App) Shutdown() {}

// HandleMethod dispatches one RPC call and returns the response envelope.
func (a *App) HandleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case methodRegister, methodReconfigure:
		return a.register(request)
	case methodQuiesce, methodShutdown:
		return okEnvelope(struct{}{})
	case methodPatchBodyBefore:
		return okEnvelope(a.patchBody(request))
	case methodInterceptBefore:
		return a.interceptBefore(request)
	case methodInterceptAfter:
		return okEnvelope(struct{}{})
	default:
		return ErrorEnvelope("unknown_method", "unknown method: "+method, http.StatusNotFound), nil
	}
}

func (a *App) register(request []byte) ([]byte, error) {
	var lifecycle lifecycleRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &lifecycle); err != nil {
			return nil, fmt.Errorf("decode lifecycle request: %w", err)
		}
	}
	cfg, warnings, err := ParseConfig(lifecycle.ConfigYAML)
	if err != nil {
		a.log("warn", "cpa-stego-guard config is invalid, using defaults: "+err.Error())
		cfg = DefaultConfig()
	}
	for _, warning := range warnings {
		a.log("warn", "cpa-stego-guard config warning: "+warning)
	}
	a.config.Store(&cfg)
	fields := map[string]any{
		"mode":                string(cfg.Mode),
		"clean_quote_markers": cfg.CleanQuoteMarkers,
		"clean_region_marker": cfg.CleanRegionMarker,
		"hook":                hookFor(cfg.Mode),
	}
	a.log("info", "cpa-stego-guard configured "+formatFields(fields, configuredFieldOrder))
	return okEnvelope(registration{
		SchemaVersion: SchemaVersion,
		Metadata: metadata{
			Name:             Name,
			Version:          Version,
			Author:           Author,
			GitHubRepository: Repository,
			ConfigFields:     configFields(),
		},
		Capabilities: capabilitiesFor(cfg.Mode),
	})
}

// capabilitiesFor picks the hook for a mode. Only request.intercept_before can
// reject a request, so block mode uses it, at the cost of receiving the body
// JSON-encoded. Every other mode uses request.patch_body_before, which receives
// the raw body and returns byte-range edits the host splices in place.
func capabilitiesFor(mode Mode) capabilities {
	if mode == ModeBlock {
		return capabilities{RequestInterceptor: true, RequestInterceptorOmitBodyAfter: true}
	}
	return capabilities{RequestInterceptorPatchBodyBefore: true}
}

func hookFor(mode Mode) string {
	if mode == ModeBlock {
		return methodInterceptBefore
	}
	return methodPatchBodyBefore
}

// patchBody handles request.patch_body_before. The hook carries no protocol
// metadata, so stego.Scan identifies Anthropic Messages bodies by shape.
func (a *App) patchBody(body []byte) patchResponse {
	cfg := a.Config()
	if cfg.Mode == ModeOff || len(body) == 0 {
		return patchResponse{}
	}
	if cfg.Mode == ModeBlock {
		// A request that raced a reconfiguration to block mode cannot be
		// rejected from this hook; it is logged and forwarded unchanged.
		cfg.Mode = ModeLog
	}
	decision := Decide(cfg, stego.Scan(body))
	a.logDecision(cfg, decision, methodPatchBodyBefore, "")
	return patchResponse{Edits: decision.Edits}
}

// interceptBefore handles request.intercept_before, registered in block mode.
func (a *App) interceptBefore(request []byte) ([]byte, error) {
	cfg := a.Config()
	if cfg.Mode == ModeOff {
		return okEnvelope(struct{}{})
	}
	var req interceptRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, fmt.Errorf("decode intercept request: %w", err)
	}
	if req.SourceFormat != sourceFormatClaude || len(req.Body) == 0 {
		return okEnvelope(struct{}{})
	}
	decision := Decide(cfg, stego.Scan(req.Body))
	a.logDecision(cfg, decision, methodInterceptBefore, req.RequestID)
	resp := interceptResponse{BodyEdits: decision.Edits}
	if decision.Action == ActionBlocked {
		resp = interceptResponse{
			Terminate:       true,
			StatusCode:      http.StatusBadRequest,
			ResponseHeaders: map[string][]string{"Content-Type": {"application/json"}},
			ResponseBody:    blockResponseBody(decision.Reasons),
		}
	}
	return okEnvelope(resp)
}

// logDecision records marker counts through host.log. It never logs prompt text.
func (a *App) logDecision(cfg Config, decision Decision, hook, requestID string) {
	if decision.Action == ActionNone {
		return
	}
	c := decision.Counts
	fields := map[string]any{
		"mode":                string(cfg.Mode),
		"action":              string(decision.Action),
		"hook":                hook,
		"clean_quote_markers": cfg.CleanQuoteMarkers,
		"clean_region_marker": cfg.CleanRegionMarker,
		"date_lines":          c.DateLines,
		"quote_u2019_domain":  c.QuoteDomain,
		"quote_u02bc_lab":     c.QuoteLab,
		"quote_u02b9_both":    c.QuoteBoth,
		"region_slash_date":   c.RegionSlash,
		"stray_u02bc":         c.StrayLab,
		"stray_u02b9":         c.StrayBoth,
		"edits":               len(decision.Edits),
	}
	if requestID != "" {
		fields["plugin_request_id"] = requestID
	}
	level := "info"
	message := "cpa-stego-guard found steganographic markers"
	if c.QuoteMarkers() == 0 && c.RegionSlash == 0 {
		message = "cpa-stego-guard found U+02BC/U+02B9 outside the date line"
	}
	if decision.Action == ActionBlocked {
		level = "warn"
	}
	// CLIProxyAPI's text log prints only its own whitelisted field names, so
	// the counts go into the message itself.
	a.log(level, message+" "+formatFields(fields, decisionFieldOrder))
}

var configuredFieldOrder = []string{"mode", "hook", "clean_quote_markers", "clean_region_marker"}

var decisionFieldOrder = []string{
	"action", "mode", "hook", "date_lines", "quote_u2019_domain", "quote_u02bc_lab", "quote_u02b9_both",
	"region_slash_date", "stray_u02bc", "stray_u02b9", "edits", "clean_quote_markers", "clean_region_marker",
	"plugin_request_id",
}

// formatFields renders fields as key=value pairs in a fixed order.
func formatFields(fields map[string]any, order []string) string {
	parts := make([]string, 0, len(order))
	for _, key := range order {
		if value, ok := fields[key]; ok {
			parts = append(parts, fmt.Sprintf("%s=%v", key, value))
		}
	}
	return strings.Join(parts, " ")
}

func (a *App) log(level, message string) {
	a.hostMu.RLock()
	host := a.host
	a.hostMu.RUnlock()
	if host == nil {
		return
	}
	_, _ = host(methodHostLog, hostLogRequest{Level: level, Message: message})
}
