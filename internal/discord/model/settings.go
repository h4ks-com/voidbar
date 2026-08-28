package model

import (
	"encoding/json"
	"math"
	"strconv"
)

// DefaultUserSettings is the settings object served to clients that
// expect a fully-formed user_settings map (web clients validate the
// READY payload with zod and reject partial objects). Persisted
// patches are merged over these defaults at read time, so the map only
// needs to list the keys clients actually read.
func DefaultUserSettings() map[string]any {
	return map[string]any{
		"theme":                    "dark",
		"locale":                   "en-US",
		"message_display_compact":  false,
		"developer_mode":           false,
		"convert_emoticons":        true,
		"show_current_game":        true,
		"restricted_guilds":        []any{},
		"guild_folders":            []any{},
		"inline_attachment_media":  true,
		"inline_embed_media":       true,
		"render_embeds":            true,
		"render_reactions":         true,
		"animate_emoji":            true,
		"afk_timeout":              300,
		"default_guilds_restricted": false,
		"streamer_mode":            false,
		"inline_filter_explicit_content": false,
	}
}

// SettingsWithDefaults overlays persisted settings on the defaults;
// nil/empty persisted maps yield the plain defaults.
func SettingsWithDefaults(persisted map[string]any) map[string]any {
	out := DefaultUserSettings()
	for k, v := range persisted {
		out[k] = v
	}
	out["theme"] = SanitizeTheme(out["theme"])
	out["guild_folders"] = normalizeGuildFolders(out["guild_folders"])
	return out
}

// SanitizeTheme clamps the theme to the intersection of client
// generations: old Android clients (126.21) only know dark/light/pureEvil
// while web schemas know dark/light/darker/midnight. Serving a value
// outside the intersection in READY sent the Android client into an
// AppActivity relaunch loop (its theme resolution never settles), so
// reads normalize: light stays light, everything else becomes dark.
func SanitizeTheme(v any) string {
	if s, ok := v.(string); ok && s == "light" {
		return "light"
	}
	return "dark"
}

// normalizeGuildFolders coerces folder ids and guild_ids entries to their
// exact string form. The Android client PATCHes snowflakes as JSON
// numbers; with UseNumber decoding their digits survive exactly and are
// stringified here. Float64 values (decoded without UseNumber) only
// recover below 2^53 - a mangled snowflake stays dropped rather than
// served as an id referencing a nonexistent guild. Folders left with no
// resolvable guilds are dropped entirely.
func normalizeGuildFolders(v any) []any {
	folders, ok := v.([]any)
	if !ok {
		return []any{}
	}
	out := make([]any, 0, len(folders))
	for _, f := range folders {
		m, ok := f.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := idToString(m["id"]); ok {
			m["id"] = id
		}
		if ids, ok := m["guild_ids"].([]any); ok {
			keep := make([]any, 0, len(ids))
			for _, id := range ids {
				if s, ok := idToString(id); ok {
					keep = append(keep, s)
				}
			}
			if len(keep) == 0 {
				continue
			}
			m["guild_ids"] = keep
		}
		out = append(out, m)
	}
	return out
}

// idToString returns the exact string form of an id when recoverable:
// strings pass through, json.Number yields its literal digits (integer
// values only), float64 survives only where integral and within 2^53.
func idToString(v any) (string, bool) {
	switch n := v.(type) {
	case string:
		return n, true
	case json.Number:
		s := n.String()
		if _, err := strconv.ParseInt(s, 10, 64); err == nil {
			return s, true
		}
	case float64:
		if n == math.Trunc(n) && math.Abs(n) <= 1<<53 {
			return strconv.FormatInt(int64(n), 10), true
		}
	}
	return "", false
}
