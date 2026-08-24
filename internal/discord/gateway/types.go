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
	V                    int                `json:"v"`
	User                 *model.User        `json:"user"`
	Guilds               []guildUnavailable `json:"guilds"`
	SessionID            string             `json:"session_id"`
	ResumeURL            string             `json:"resume_url"`
	PrivateChannels      []any              `json:"private_channels"`
	Presences            []any              `json:"presences"`
	Relationships        []any              `json:"relationships"`
	GeoOrderedRTCRegions []any              `json:"geo_ordered_rtc_regions"`
	SessionType          string             `json:"session_type"`
	UserSettings         map[string]any     `json:"user_settings"`
}
