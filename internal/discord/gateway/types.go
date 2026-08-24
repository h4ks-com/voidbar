package gateway

import (
	"encoding/json"

	"github.com/h4ks-com/voidbar/internal/discord/model"
)

type Payload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

type opFrame struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
}

type IdentifyData struct {
	Token      string          `json:"token"`
	Properties json.RawMessage `json:"properties,omitempty"`
	Intents    *int            `json:"intents,omitempty"`
}

type ResumeData struct {
	Token     string `json:"token"`
	SessionID string `json:"session_id"`
	Seq       int64  `json:"seq"`
}

type HelloData struct {
	HeartbeatInterval int      `json:"heartbeat_interval"`
	Trace             []string `json:"_trace"`
}

type guildUnavailable struct {
	ID          string `json:"id"`
	Unavailable bool   `json:"unavailable"`
}

type ReadyData struct {
	V                    int             `json:"v"`
	User                 *model.User     `json:"user"`
	Guilds               []any           `json:"guilds"`
	SessionID            string          `json:"session_id"`
	ResumeURL            string          `json:"resume_url"`
	ResumeGatewayURL     string          `json:"resume_gateway_url"`
	PrivateChannels      []any           `json:"private_channels"`
	Users                []any           `json:"users"`
	Presences            []any           `json:"presences"`
	Relationships        []any           `json:"relationships"`
	Sessions             []any           `json:"sessions"`
	GeoOrderedRTCRegions []any           `json:"geo_ordered_rtc_regions"`
	SessionType          string          `json:"session_type"`
	UserSettings         map[string]any  `json:"user_settings"`
	Experiments          []any           `json:"experiments"`
	GuildExperiments     []any           `json:"guild_experiments"`
	UserGuildSettings    *VersionedArray `json:"user_guild_settings"`
	ReadState            *VersionedArray `json:"read_state"`
	// UserSettingsProto is the base64-encoded serialized PreloadedUserSettings
	// protobuf (wire format is a STRING; the client decodes it via
	// b64ToPreloadedUserSettingsProto before storing). An empty string is a
	// valid empty message meaning "all defaults".
	UserSettingsProto string         `json:"user_settings_proto"`
	ConnectedAccounts []any          `json:"connected_accounts"`
	GuildJoinRequests []any          `json:"guild_join_requests"`
	Consents          map[string]any `json:"consents"`
	AnalyticsToken    string         `json:"analytics_token"`
}

// VersionedArray is Discord's {entries, partial, version} wrapper used by
// READY fields such as user_guild_settings and read_state. Entries must
// always be present.
type VersionedArray struct {
	Entries []any `json:"entries"`
	Partial bool  `json:"partial"`
	Version int   `json:"version"`
}
