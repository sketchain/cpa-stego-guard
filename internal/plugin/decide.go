package plugin

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sketchain/cpa-stego-guard/internal/stego"
)

// Action is what the plugin did with a request.
type Action string

const (
	ActionNone    Action = "none"
	ActionCleaned Action = "cleaned"
	ActionBlocked Action = "blocked"
	ActionLogged  Action = "logged"
)

// Decision is the outcome of applying a configuration to a scan report.
type Decision struct {
	Action Action
	Counts stego.Counts
	Edits  []bodyEdit
	// Reasons lists the marker types that caused a block, in a fixed order.
	Reasons []string
}

// Decide applies cfg to report. It is a pure function: the same inputs always
// produce the same decision, and the edits are ordered by offset.
func Decide(cfg Config, report stego.Report) Decision {
	counts := report.Counts()
	decision := Decision{Action: ActionNone, Counts: counts}
	if cfg.Mode == ModeOff || !report.Messages {
		return decision
	}
	quoteActive := cfg.CleanQuoteMarkers && counts.QuoteMarkers() > 0
	regionActive := cfg.CleanRegionMarker && counts.RegionSlash > 0

	switch cfg.Mode {
	case ModeClean:
		for _, line := range report.DateLines {
			if cfg.CleanQuoteMarkers && line.Quote.Marked() {
				decision.Edits = append(decision.Edits, bodyEdit{Start: line.QuoteSpan.Start, End: line.QuoteSpan.End, Replacement: "'"})
			}
			if cfg.CleanRegionMarker && line.Slash {
				for _, sep := range line.SeparatorSpans {
					decision.Edits = append(decision.Edits, bodyEdit{Start: sep.Start, End: sep.End, Replacement: "-"})
				}
			}
		}
		if len(decision.Edits) > 0 {
			decision.Action = ActionCleaned
		}
	case ModeBlock:
		if quoteActive || regionActive {
			decision.Action = ActionBlocked
			decision.Reasons = blockReasons(counts, cfg)
		}
	}
	if decision.Action == ActionNone && counts.Any() {
		decision.Action = ActionLogged
	}
	return decision
}

func blockReasons(c stego.Counts, cfg Config) []string {
	var reasons []string
	if cfg.CleanQuoteMarkers {
		if c.QuoteDomain > 0 {
			reasons = append(reasons, "日期行 Today 后的撇号是 U+2019（命中域名名单）")
		}
		if c.QuoteLab > 0 {
			reasons = append(reasons, "日期行 Today 后的撇号是 U+02BC（命中 AI 实验室关键词）")
		}
		if c.QuoteBoth > 0 {
			reasons = append(reasons, "日期行 Today 后的撇号是 U+02B9（域名名单和 AI 实验室关键词都命中）")
		}
	}
	if cfg.CleanRegionMarker && c.RegionSlash > 0 {
		reasons = append(reasons, "日期行使用斜杠日期 YYYY/MM/DD（中国时区标记）")
	}
	return reasons
}

// blockResponseBody builds an Anthropic-style error body for a blocked request.
func blockResponseBody(reasons []string) []byte {
	message := fmt.Sprintf(
		"请求已被 cpa-stego-guard 拦截，未转发上游。检测到 Claude Code 隐写标记：%s。"+
			"Claude Code 2.1.91–2.1.196 在 ANTHROPIC_BASE_URL 不是官方地址时会写入这些标记，"+
			"2.1.293 起已移除。请把 Claude Code 升级到 2.1.293 或更高版本后重试。"+
			" / Blocked by cpa-stego-guard: Claude Code steganographic markers found in the date line. "+
			"Please upgrade Claude Code to 2.1.293 or later.",
		strings.Join(reasons, "；"),
	)
	raw, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "invalid_request_error",
			"message": message,
		},
	})
	return raw
}
