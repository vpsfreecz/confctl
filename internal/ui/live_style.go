package ui

import "strings"

// LiveText styles only sanitized confctl text with this finite palette. Child
// records use the plain log path and cannot provide SGR or terminal controls.
func LiveText(text, style string, color bool) string {
	return liveStyle(liveSafe(text), style, color)
}

func liveStyle(text, style string, color bool) string {
	if !color || text == "" {
		return text
	}
	code := ""
	switch style {
	case "bright":
		code = "1"
	case "yellow":
		code = "33"
	case "cyan":
		code = "36"
	case "green":
		code = "32"
	case "red":
		code = "31"
	}
	if code == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

// Append one semantic field after sanitation and column clipping, then wrap
// that field in balanced confctl SGR. A negative remainder means no clipping.
func liveField(out *strings.Builder, text, style string, remaining *int, color bool) {
	text = liveSafe(text)
	if *remaining >= 0 {
		text = liveClip(text, *remaining)
		*remaining -= liveWidth(text)
	}
	out.WriteString(liveStyle(text, style, color))
}
