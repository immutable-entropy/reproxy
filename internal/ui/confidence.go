package ui

import (
	"fmt"

	"reproxy/internal/findings"
)

// Confidence is a 0–100 score of how likely a finding is a true positive (see
// findings.Finding.Confidence). These helpers turn a score into the words the
// Findings views show beside it, so the dialog, the list and the detail pane
// read it the same way.

// confidenceWord names the band a score falls in, for a label beside the
// number.
func confidenceWord(c int) string {
	switch {
	case c >= 85:
		return "very likely real"
	case c > findings.ConfidenceUnknown:
		return "likely real"
	case c == findings.ConfidenceUnknown:
		return "unknown"
	case c >= 15:
		return "likely noise"
	default:
		return "very likely noise"
	}
}

// confidenceText is a score and its band together: "likely real (82)", or
// "not reviewed" for a finding with no score yet.
func confidenceText(c *int) string {
	if c == nil {
		return "not reviewed"
	}
	return fmt.Sprintf("%s (%d)", confidenceWord(*c), *c)
}

// confidenceNote is confidenceText for a list subtitle, left empty while the
// finding is unreviewed so it shows no clutter until it has been scored.
func confidenceNote(c *int) string {
	if c == nil {
		return ""
	}
	return confidenceText(c)
}

// Concern is a 0–100 score of how big an issue a finding is (see
// findings.Finding.Concern): 0 no concern, 100 critical. These mirror the
// confidence helpers.

// concernWord names the band a concern score falls in.
func concernWord(c int) string {
	switch {
	case c >= 75:
		return "critical"
	case c >= 50:
		return "high"
	case c >= 25:
		return "moderate"
	case c > 0:
		return "low"
	default:
		return "none"
	}
}

// concernText is a concern score and its band together, or "not assessed".
func concernText(c *int) string {
	if c == nil {
		return "not assessed"
	}
	return fmt.Sprintf("%s (%d)", concernWord(*c), *c)
}

// concernNote is concernText for a list subtitle, empty until assessed.
func concernNote(c *int) string {
	if c == nil {
		return ""
	}
	return concernText(c)
}

// concernColor is the band colour for a concern score: a higher concern is
// more alarming, so it climbs from success through held to serverError.
func concernColor(c int) string {
	switch {
	case c >= 70:
		return hex(colorServerError)
	case c >= 40:
		return hex("held")
	default:
		return hex(colorSuccess)
	}
}
