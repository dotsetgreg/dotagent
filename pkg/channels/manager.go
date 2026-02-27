// DotAgent - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 DotAgent contributors

package channels

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dotsetgreg/dotagent/pkg/bus"
	"github.com/dotsetgreg/dotagent/pkg/config"
	"github.com/dotsetgreg/dotagent/pkg/constants"
	"github.com/dotsetgreg/dotagent/pkg/logger"
)

type Manager struct {
	channels     map[string]Channel
	bus          *bus.MessageBus
	config       *config.Config
	dispatchTask *asyncTask
	mu           sync.RWMutex
}

type asyncTask struct {
	cancel context.CancelFunc
}

const (
	sendRetryAttempts = 3
	sendRetryDelay    = 300 * time.Millisecond
)

func NewManager(cfg *config.Config, messageBus *bus.MessageBus) (*Manager, error) {
	m := &Manager{
		channels: make(map[string]Channel),
		bus:      messageBus,
		config:   cfg,
	}

	if err := m.initChannels(); err != nil {
		return nil, err
	}

	return m, nil
}

func (m *Manager) initChannels() error {
	logger.InfoC("channels", "Initializing channel manager")

	if strings.TrimSpace(m.config.Channels.Discord.Token) == "" {
		return fmt.Errorf("channels.discord.token is required")
	}

	logger.DebugC("channels", "Attempting to initialize Discord channel")
	discord, err := NewDiscordChannel(m.config.Channels.Discord, m.bus)
	if err != nil {
		return fmt.Errorf("initialize Discord channel: %w", err)
	}
	m.channels["discord"] = discord
	logger.InfoC("channels", "Discord channel initialized successfully")

	logger.InfoCF("channels", "Channel initialization completed", map[string]interface{}{
		"enabled_channels": len(m.channels),
	})

	return nil
}

func (m *Manager) StartAll(ctx context.Context) error {
	m.mu.RLock()
	if len(m.channels) == 0 {
		m.mu.RUnlock()
		logger.WarnC("channels", "No channels enabled")
		return nil
	}
	channelsCopy := make(map[string]Channel, len(m.channels))
	for name, channel := range m.channels {
		channelsCopy[name] = channel
	}
	m.mu.RUnlock()

	logger.InfoC("channels", "Starting all channels")

	var started []string
	var startErrors []string
	for name, channel := range channelsCopy {
		logger.InfoCF("channels", "Starting channel", map[string]interface{}{"channel": name})
		if err := channel.Start(ctx); err != nil {
			logger.ErrorCF("channels", "Failed to start channel", map[string]interface{}{
				"channel": name,
				"error":   err.Error(),
			})
			startErrors = append(startErrors, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		started = append(started, name)
	}

	if len(startErrors) > 0 {
		for _, name := range started {
			channel := channelsCopy[name]
			if err := channel.Stop(ctx); err != nil {
				logger.WarnCF("channels", "Failed to stop partially-started channel", map[string]interface{}{
					"channel": name,
					"error":   err.Error(),
				})
			}
		}
		return fmt.Errorf("failed to start channels: %s", strings.Join(startErrors, "; "))
	}

	dispatchCtx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	if m.dispatchTask != nil {
		m.dispatchTask.cancel()
	}
	m.dispatchTask = &asyncTask{cancel: cancel}
	m.mu.Unlock()

	go m.dispatchOutbound(dispatchCtx)

	logger.InfoCF("channels", "All channels started", map[string]interface{}{
		"count": len(started),
	})
	return nil
}

func (m *Manager) StopAll(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	logger.InfoC("channels", "Stopping all channels")

	if m.dispatchTask != nil {
		m.dispatchTask.cancel()
		m.dispatchTask = nil
	}

	for name, channel := range m.channels {
		logger.InfoCF("channels", "Stopping channel", map[string]interface{}{
			"channel": name,
		})
		if err := channel.Stop(ctx); err != nil {
			logger.ErrorCF("channels", "Error stopping channel", map[string]interface{}{
				"channel": name,
				"error":   err.Error(),
			})
		}
	}

	logger.InfoC("channels", "All channels stopped")
	return nil
}

func (m *Manager) dispatchOutbound(ctx context.Context) {
	logger.InfoC("channels", "Outbound dispatcher started")

	for {
		select {
		case <-ctx.Done():
			logger.InfoC("channels", "Outbound dispatcher stopped")
			return
		default:
			msg, ok := m.bus.SubscribeOutbound(ctx)
			if !ok {
				continue
			}

			// Silently skip internal channels
			if constants.IsInternalChannel(msg.Channel) {
				continue
			}

			m.mu.RLock()
			channel, exists := m.channels[msg.Channel]
			m.mu.RUnlock()

			if !exists {
				logger.WarnCF("channels", "Unknown channel for outbound message", map[string]interface{}{
					"channel": msg.Channel,
				})
				continue
			}

			if err := m.sendWithRetry(ctx, channel, msg); err != nil {
				logger.ErrorCF("channels", "Error sending message to channel", map[string]interface{}{
					"channel": msg.Channel,
					"error":   err.Error(),
				})
			}
		}
	}
}

func (m *Manager) sendWithRetry(ctx context.Context, channel Channel, msg bus.OutboundMessage) error {
	var err error
	attempts := sendRetryAttempts
	if attempts <= 0 {
		attempts = 1
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		err = channel.Send(ctx, msg)
		if err == nil {
			return nil
		}
		if attempt == attempts {
			break
		}
		if !isTransientSendError(err) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * sendRetryDelay):
		}
	}
	if strings.TrimSpace(msg.ChatID) != "" {
		fallback := bus.OutboundMessage{
			Channel: msg.Channel,
			ChatID:  msg.ChatID,
			Content: "⚠️ Delivery failed after retries. Please ask me to resend that response.",
		}
		if fallbackErr := channel.Send(ctx, fallback); fallbackErr != nil {
			logger.ErrorCF("channels", "Failed to send terminal delivery failure notice", map[string]interface{}{
				"channel": msg.Channel,
				"chat_id": msg.ChatID,
				"error":   fallbackErr.Error(),
			})
		}
	}
	return err
}

func isTransientSendError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "tempor") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "429") ||
		strings.Contains(msg, "5xx")
}

func (m *Manager) GetChannel(name string) (Channel, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	channel, ok := m.channels[name]
	return channel, ok
}

func (m *Manager) GetStatus() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	status := make(map[string]interface{})
	for name, channel := range m.channels {
		status[name] = map[string]interface{}{
			"enabled": true,
			"running": channel.IsRunning(),
		}
	}
	return status
}

func (m *Manager) GetEnabledChannels() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.channels))
	for name := range m.channels {
		names = append(names, name)
	}
	return names
}

func (m *Manager) RegisterChannel(name string, channel Channel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.channels[name] = channel
}

func (m *Manager) UnregisterChannel(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.channels, name)
}

func (m *Manager) SendToChannel(ctx context.Context, channelName, chatID, content string) error {
	m.mu.RLock()
	channel, exists := m.channels[channelName]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("channel %s not found", channelName)
	}

	msg := bus.OutboundMessage{
		Channel: channelName,
		ChatID:  chatID,
		Content: content,
	}

	return channel.Send(ctx, msg)
}
