package main

// protocol.go is the single description of Anchor's wire format.
//
// A packet is one null-terminated JSON object. Anchor is a relay: packets are
// defined by the game mod and forwarded verbatim (Envelope.Raw). The server
// only ever reads the envelope fields below, and only ever originates the
// packet types at the bottom of this file.

import "encoding/json"

// Packet types the server interprets. Any other type is relayed untouched.
const (
	PacketHandshake         = "HANDSHAKE"
	PacketStats             = "STATS"
	PacketUpdateClientState = "UPDATE_CLIENT_STATE"
	PacketUpdateRoomState   = "UPDATE_ROOM_STATE"
	PacketUpdateTeamState   = "UPDATE_TEAM_STATE"
	PacketRequestTeamState  = "REQUEST_TEAM_STATE"
	PacketGameComplete      = "GAME_COMPLETE"
	PacketAllClientState    = "ALL_CLIENT_STATE"
	PacketServerMessage     = "SERVER_MESSAGE"
	PacketDisableAnchor     = "DISABLE_ANCHOR"
	PacketHeartbeat         = "HEARTBEAT"
)

// Envelope holds every field of an incoming packet the server ever looks at,
// parsed exactly once per packet. Raw is the packet as received, relayed
// verbatim so client-defined payload fields survive untouched.
type Envelope struct {
	Type           string          `json:"type"`
	RoomID         string          `json:"roomId"`
	ClientID       uint64          `json:"clientId"`
	TargetClientID *uint64         `json:"targetClientId"`
	TargetTeamID   *string         `json:"targetTeamId"`
	AddToQueue     bool            `json:"addToQueue"`
	Quiet          bool            `json:"quiet"`
	State          json.RawMessage `json:"state"`
	ClientState    json.RawMessage `json:"clientState"`
	RoomState      json.RawMessage `json:"roomState"`

	Raw string `json:"-"`
}

func parseEnvelope(raw string) (*Envelope, error) {
	env := &Envelope{Raw: raw}
	if err := json.Unmarshal([]byte(raw), env); err != nil {
		return nil, err
	}
	return env, nil
}

// clientStateFields are the pieces of the otherwise opaque client state blob
// that the server makes decisions on. The blob itself is stored and relayed
// as-is.
type clientStateFields struct {
	TeamID       string `json:"teamId"`
	IsSaveLoaded bool   `json:"isSaveLoaded"`
}

func parseClientStateFields(state json.RawMessage) clientStateFields {
	var f clientStateFields
	json.Unmarshal(state, &f)
	return f
}

// Packets the server originates.

func marshalPacket(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

type statsReplyPacket struct {
	Type              string `json:"type"`
	UniqueCount       uint64 `json:"uniqueCount"`
	GameCompleteCount uint64 `json:"gameCompleteCount"`
	OnlineCount       int    `json:"onlineCount"`
}

type serverMessagePacket struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type disableAnchorPacket struct {
	Type string `json:"type"`
}

// teamStateReplyPacket answers REQUEST_TEAM_STATE when no live teammate can:
// the last saved team state (omitted when there is none) plus queued packets.
type teamStateReplyPacket struct {
	Type  string          `json:"type"`
	State json.RawMessage `json:"state,omitempty"`
	Queue []string        `json:"queue"`
}

const heartbeatPacket = `{"type":"HEARTBEAT","quiet":true}`
