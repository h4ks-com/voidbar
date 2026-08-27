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
	return out
}
