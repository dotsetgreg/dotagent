package bus

type InboundMessage struct {
	Channel    string            `json:"channel"`
	SenderID   string            `json:"sender_id"`
	ChatID     string            `json:"chat_id"`
	Content    string            `json:"content"`
	Media      []string          `json:"media,omitempty"`
	SessionKey string            `json:"session_key"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

type OutboundMessage struct {
	Channel     string `json:"channel"`
	ChatID      string `json:"chat_id"`
	Content     string `json:"content"`
	Stream      bool   `json:"stream,omitempty"`
	StreamID    string `json:"stream_id,omitempty"`
	StreamFinal bool   `json:"stream_final,omitempty"`
}

type EventMessage struct {
	Type       string            `json:"type"`
	SessionKey string            `json:"session_key,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

type MessageHandler func(InboundMessage) error
