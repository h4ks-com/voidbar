package model

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
	out["guild_folders"] = normalizeGuildFolders(out["guild_folders"])
	return out
}

// normalizeGuildFolders drops guild_ids entries that are not strings.
// The Android client PATCHes snowflakes as JSON numbers; once unmarshaled
// into float64 they have already lost precision (snowflakes exceed 2^53),
// so they cannot be recovered and are discarded instead of being served
// as wrong ids that would reference nonexistent guilds. Folders left with
// no resolvable guilds are dropped entirely.
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
		if ids, ok := m["guild_ids"].([]any); ok {
			keep := make([]any, 0, len(ids))
			for _, id := range ids {
				if _, ok := id.(string); ok {
					keep = append(keep, id)
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
