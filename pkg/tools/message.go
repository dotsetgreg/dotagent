package tools

import (
	"context"
	"fmt"
	"sync"
)

type SendCallback func(channel, chatID, content string) error

type MessageTool struct {
	sendCallback   SendCallback
	defaultChannel string
	defaultChatID  string
	mu             sync.RWMutex
}

func NewMessageTool() *MessageTool {
	return &MessageTool{}
}

func (t *MessageTool) Name() string {
	return "message"
}

func (t *MessageTool) Description() string {
	return "Send a message to user on a chat channel. Use this when you want to communicate something."
}

func (t *MessageTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"content": map[string]interface{}{
				"type":        "string",
				"description": "The message content to send",
			},
			"channel": map[string]interface{}{
				"type":        "string",
				"description": "Optional: target channel (defaults to discord in current session)",
			},
			"chat_id": map[string]interface{}{
				"type":        "string",
				"description": "Optional: target chat/user ID",
			},
		},
		"required": []string{"content"},
	}
}

func (t *MessageTool) SetContext(channel, chatID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.defaultChannel = channel
	t.defaultChatID = chatID
}

func (t *MessageTool) SetSendCallback(callback SendCallback) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sendCallback = callback
}

func (t *MessageTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	content, ok := args["content"].(string)
	if !ok {
		return &ToolResult{ForLLM: "content is required", IsError: true}
	}

	channel, _ := args["channel"].(string)
	chatID, _ := args["chat_id"].(string)
	ctxChannel, ctxChatID := channelChatFromContext(ctx)

	if channel == "" {
		channel = ctxChannel
	}
	if channel == "" {
		t.mu.RLock()
		channel = t.defaultChannel
		t.mu.RUnlock()
	}
	if chatID == "" {
		chatID = ctxChatID
	}
	if chatID == "" {
		t.mu.RLock()
		chatID = t.defaultChatID
		t.mu.RUnlock()
	}

	if channel == "" || chatID == "" {
		return &ToolResult{ForLLM: "No target channel/chat specified", IsError: true}
	}

	t.mu.RLock()
	sendCallback := t.sendCallback
	t.mu.RUnlock()

	if sendCallback == nil {
		return &ToolResult{ForLLM: "Message sending not configured", IsError: true}
	}

	if err := sendCallback(channel, chatID, content); err != nil {
		return &ToolResult{
			ForLLM:  fmt.Sprintf("sending message: %v", err),
			IsError: true,
			Err:     err,
		}
	}

	markMessageSentInContext(ctx)
	// Silent: user already received the message directly
	return &ToolResult{
		ForLLM: fmt.Sprintf("Message sent to %s:%s", channel, chatID),
		Silent: true,
	}
}
