package plugin

import (
	"fmt"
	"strings"

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
}

// DefaultConfig is used for omitted keys.
func DefaultConfig() Config {
	return Config{Mode: ModeClean, CleanQuoteMarkers: true, CleanRegionMarker: true}
}

type rawConfig struct {
	Mode              *string `yaml:"mode"`
	CleanQuoteMarkers *bool   `yaml:"clean_quote_markers"`
	CleanRegionMarker *bool   `yaml:"clean_region_marker"`
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
	}
}
