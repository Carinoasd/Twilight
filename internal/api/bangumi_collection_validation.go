package api

import (
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

func strictBangumiInt(raw any, low, high int) (int, bool) {
	value, ok := raw.(float64)
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value || value < float64(low) || value > float64(high) {
		return 0, false
	}
	return int(value), true
}
func bangumiCollectionExtras(payload map[string]any) (map[string]any, bool) {
	out := map[string]any{}
	if value, exists := payload["comment"]; exists {
		text, ok := value.(string)
		if !ok || utf8.RuneCountInString(text) > 1000 {
			return nil, false
		}
		out["comment"] = text
	}
	if value, exists := payload["private"]; exists {
		private, ok := value.(bool)
		if !ok {
			return nil, false
		}
		out["private"] = private
	}
	if value, exists := payload["tags"]; exists {
		tags, ok := value.([]any)
		if !ok || len(tags) > 10 {
			return nil, false
		}
		clean := []string{}
		seen := map[string]bool{}
		for _, value := range tags {
			tag, ok := value.(string)
			tag = strings.TrimSpace(tag)
			if !ok || tag == "" || utf8.RuneCountInString(tag) > 30 || strings.IndexFunc(tag, unicode.IsSpace) >= 0 {
				return nil, false
			}
			if !seen[tag] {
				clean = append(clean, tag)
				seen[tag] = true
			}
		}
		out["tags"] = clean
	}
	return out, true
}

func bangumiPublicTags(raw any) []string {
	out := []string{}
	switch tags := raw.(type) {
	case []any:
		for _, value := range tags {
			if tag, ok := value.(string); ok && utf8.RuneCountInString(tag) <= 30 {
				out = append(out, tag)
				if len(out) == 10 {
					break
				}
			}
		}
	case []string:
		for _, tag := range tags {
			if utf8.RuneCountInString(tag) <= 30 {
				out = append(out, tag)
				if len(out) == 10 {
					break
				}
			}
		}
	}
	return out
}
