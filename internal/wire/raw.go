package wire

import "encoding/json"

// rawJSON embeds stored JSON (an event's data) without re-encoding it.
func rawJSON(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return json.RawMessage(b)
}
