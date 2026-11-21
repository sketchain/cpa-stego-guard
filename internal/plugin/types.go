// Package plugin implements the CLIProxyAPI v8 plugin contract for
// cpa-stego-guard on top of package stego. It has no cgo dependency; the C ABI
// lives in cmd/cpa-stego-guard.
package plugin

import "encoding/json"

const (
	// ABIVersion is the native C ABI version of CLIProxyAPI v8 plugins.
	ABIVersion uint32 = 1
	// SchemaVersion is the RPC contract this plugin speaks. Version 8 is the
	// first one with raw request body patching (request.patch_body_before).
	SchemaVersion uint32 = 8
)

// Plugin metadata reported at registration.
const (
	Name       = "CPA Stego Guard"
	Version    = "0.1.0"
	Author     = "sketchain"
	Repository = "https://github.com/sketchain/cpa-stego-guard"
)

// RPC method names used by this plugin.
const (
	methodRegister        = "plugin.register"
	methodReconfigure     = "plugin.reconfigure"
	methodQuiesce         = "plugin.quiesce"
	methodShutdown        = "plugin.shutdown"
	methodPatchBodyBefore = "request.patch_body_before"
	methodInterceptBefore = "request.intercept_before"
	methodInterceptAfter  = "request.intercept_after"
	methodHostLog         = "host.log"
)

// Envelope is the JSON wrapper of every RPC response.
type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

// EnvelopeError is a structured RPC failure.
type EnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// ErrorEnvelope encodes a failed RPC response.
func ErrorEnvelope(code, message string, httpStatus int) []byte {
	raw, _ := json.Marshal(Envelope{OK: false, Error: &EnvelopeError{Code: code, Message: message, HTTPStatus: httpStatus}})
	return raw
}

func okEnvelope(result any) ([]byte, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Envelope{OK: true, Result: raw})
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      metadata     `json:"metadata"`
	Capabilities  capabilities `json:"capabilities"`
}

type metadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	ConfigFields     []configField `json:"ConfigFields"`
}

type configField struct {
	Name        string   `json:"Name"`
	Type        string   `json:"Type"`
	Default     any      `json:"Default"`
	EnumValues  []string `json:"EnumValues,omitempty"`
	Description string   `json:"Description"`
}

type capabilities struct {
	RequestInterceptor                bool `json:"request_interceptor,omitempty"`
	RequestInterceptorOmitBodyAfter   bool `json:"request_interceptor_omit_body_after,omitempty"`
	RequestInterceptorPatchBodyBefore bool `json:"request_interceptor_patch_body_before,omitempty"`
}

// bodyEdit is one byte-range replacement, in the shape of pluginapi.RequestBodyEdit.
type bodyEdit struct {
	Start       int    `json:"start"`
	End         int    `json:"end"`
	Replacement string `json:"replacement"`
}

type patchResponse struct {
	Edits []bodyEdit `json:"edits,omitempty"`
}

// interceptRequest mirrors the fields of pluginapi.RequestInterceptRequest this
// plugin reads. The host encodes them with Go's default field names.
type interceptRequest struct {
	RequestID    string `json:"RequestID"`
	SourceFormat string `json:"SourceFormat"`
	Body         []byte `json:"Body"`
}

// interceptResponse mirrors pluginapi.RequestInterceptResponse.
type interceptResponse struct {
	BodyEdits       []bodyEdit          `json:"body_edits,omitempty"`
	Terminate       bool                `json:"Terminate,omitempty"`
	StatusCode      int                 `json:"StatusCode,omitempty"`
	ResponseHeaders map[string][]string `json:"ResponseHeaders,omitempty"`
	ResponseBody    []byte              `json:"ResponseBody,omitempty"`
}

type hostLogRequest struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}
