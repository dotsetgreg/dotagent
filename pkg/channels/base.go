package channels

import (
	"context"
	"fmt"
	"strings"

	"github.com/dotsetgreg/dotagent/pkg/bus"
	"github.com/dotsetgreg/dotagent/pkg/logger"
)

type Channel interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Send(ctx context.Context, msg bus.OutboundMessage) error
	IsRunning() bool
	IsAllowed(senderID string) bool
}

type BaseChannel struct {
	config    interface{}
	bus       *bus.MessageBus
	running   bool
	name      string
	allowList []string
}

func NewBaseChannel(name string, config interface{}, bus *bus.MessageBus, allowList []string) *BaseChannel {
	return &BaseChannel{
		config:    config,
		bus:       bus,
		name:      name,
		allowList: allowList,
		running:   false,
	}
}

func (c *BaseChannel) Name() string {
	return c.name
}

func (c *BaseChannel) IsRunning() bool {
	return c.running
}

func (c *BaseChannel) IsAllowed(senderID string) bool {
	if len(c.allowList) == 0 {
		return true
	}

	// Extract parts from compound senderID like "123456|username"
	senderID = strings.TrimSpace(senderID)
	idPart := senderID
	userPart := ""
	if idx := strings.Index(senderID, "|"); idx > 0 {
		idPart = strings.TrimSpace(senderID[:idx])
		userPart = strings.TrimSpace(senderID[idx+1:])
	}

	for _, allowed := range c.allowList {
		raw := strings.TrimSpace(allowed)
		if raw == "" {
			continue
		}
		if strings.HasPrefix(raw, "@") {
			candidate := strings.TrimSpace(strings.TrimPrefix(raw, "@"))
			if candidate == "" {
				continue
			}
			if (userPart != "" && strings.EqualFold(candidate, userPart)) || (userPart == "" && strings.EqualFold(candidate, senderID)) {
				return true
			}
			continue
		}
		if strings.EqualFold(raw, senderID) || (idPart != "" && strings.EqualFold(raw, idPart)) {
			return true
		}
	}

	return false
}

func (c *BaseChannel) HandleMessage(senderID, chatID, messageID, content string, media []string, metadata map[string]string) {
	if !c.IsAllowed(senderID) {
		return
	}

	// Legacy session key fallback. Canonical v2 identity keys are derived
	// in the agent loop from workspace+channel+chat+actor.
	sessionKey := fmt.Sprintf("%s:%s", c.name, chatID)

	msg := bus.InboundMessage{
		Channel:    c.name,
		SenderID:   senderID,
		ChatID:     chatID,
		Content:    content,
		Media:      media,
		SessionKey: sessionKey,
		MessageID:  strings.TrimSpace(messageID),
		Metadata:   metadata,
	}

	if err := c.bus.PublishInbound(msg); err != nil {
		logger.WarnCF("channel", "Failed to publish inbound message",
			map[string]interface{}{
				"channel": c.name,
				"chat_id": chatID,
				"error":   err.Error(),
			})
	}
}

func (c *BaseChannel) setRunning(running bool) {
	c.running = running
}
