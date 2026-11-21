package plugin

import (
	"fmt"
	"strings"

	"github.com/sketchain/cpa-stego-guard/internal/stego"
	"gopkg.in/yaml.v3"
)

// Mode selects what the plugin does with a request that carries markers.
type Mode string

const (
	// ModeClean removes the markers and forwards the request.
	ModeClean Mode = "clean"
	// ModeBlock rejects the request with HTTP 400 instead of forwarding it.
	ModeBlock Mode = "block"
	// ModeLog only records the markers.
	ModeLog Mode = "log"
	// ModeOff does nothing.
	ModeOff Mode = "off"
)

// Config is the plugin configuration under plugins.configs.cpa-stego-guard.
type Config struct {
	Mode Mode
	// CleanQuoteMarkers acts on the date line apostrophes U+2019, U+02BC and
	// U+02B9 (domain list and AI lab keyword markers).
	CleanQuoteMarkers bool
	// CleanRegionMarker acts on the slash date (China time zone marker).
	CleanRegionMarker bool
	// CCVersion is the Claude Code version every billing header cc_version is
	// aligned to. Empty disables alignment. It must equal the User-Agent
	// version CLIProxyAPI sends upstream, which the plugin API does not expose.
	CCVersion string
}

// DefaultConfig is used for omitted keys.
func DefaultConfig() Config {
	return Config{Mode: ModeClean, CleanQuoteMarkers: true, CleanRegionMarker: true}
}

type rawConfig struct {
	Mode              *string `yaml:"mode"`
	CleanQuoteMarkers *bool   `yaml:"clean_quote_markers"`
	CleanRegionMarker *bool   `yaml:"clean_region_marker"`
	CCVersion         *string `yaml:"cc_version"`
}

// ParseConfig reads the plugin YAML. Omitted keys take their defaults. An
// unknown mode is reported as a warning and replaced with the default mode, so
// a typo never silently disables the guard.
func ParseConfig(raw []byte) (Config, []string, error) {
	cfg := DefaultConfig()
	var parsed rawConfig
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return cfg, nil, fmt.Errorf("parse plugin config: %w", err)
	}
	var warnings []string
	if parsed.Mode != nil {
		mode := Mode(strings.ToLower(strings.TrimSpace(*parsed.Mode)))
		switch mode {
		case ModeClean, ModeBlock, ModeLog, ModeOff:
			cfg.Mode = mode
		case "":
		default:
			warnings = append(warnings, fmt.Sprintf("unknown mode %q, using %q", *parsed.Mode, ModeClean))
		}
	}
	if parsed.CleanQuoteMarkers != nil {
		cfg.CleanQuoteMarkers = *parsed.CleanQuoteMarkers
	}
	if parsed.CleanRegionMarker != nil {
		cfg.CleanRegionMarker = *parsed.CleanRegionMarker
	}
	if parsed.CCVersion != nil {
		version := strings.TrimSpace(*parsed.CCVersion)
		switch {
		case version == "":
		case stego.ValidVersion(version):
			cfg.CCVersion = version
		default:
			warnings = append(warnings, fmt.Sprintf("cc_version %q is not a version like 2.1.321, cc_version alignment is disabled", *parsed.CCVersion))
		}
	}
	return cfg, warnings, nil
}

func configFields() []configField {
	return []configField{
		{
			Name:        "mode",
			Type:        "enum",
			Default:     string(ModeClean),
			EnumValues:  []string{string(ModeClean), string(ModeBlock), string(ModeLog), string(ModeOff)},
			Description: "clean: remove markers and forward; block: reject with HTTP 400; log: record only; off: disabled.",
		},
		{
			Name:        "clean_quote_markers",
			Type:        "boolean",
			Default:     true,
			Description: "Act on the date line apostrophes U+2019 / U+02BC / U+02B9 (domain list and AI lab keyword markers). When false they are only logged.",
		},
		{
			Name:        "clean_region_marker",
			Type:        "boolean",
			Default:     true,
			Description: "Act on the YYYY/MM/DD date (China time zone marker). When false it is only logged.",
		},
		{
			Name:    "cc_version",
			Type:    "string",
			Default: "",
			Description: "Align the billing header cc_version of every Claude Code request to this version, with a recomputed fingerprint. " +
				"Must equal the User-Agent version CLIProxyAPI sends upstream (its built-in baseline unless claude-header-defaults.user-agent is set). Empty disables alignment.",
		},
	}
}
