package channels

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/dotsetgreg/dotagent/pkg/bus"
	"github.com/dotsetgreg/dotagent/pkg/config"
	"github.com/dotsetgreg/dotagent/pkg/logger"
	"github.com/dotsetgreg/dotagent/pkg/utils"
)

const (
	sendTimeout           = 10 * time.Second
	typingRefreshInterval = 8 * time.Second
)

type DiscordChannel struct {
	*BaseChannel
	session  *discordgo.Session
	config   config.DiscordConfig
	typing   map[string]*typingSession
	typingMu sync.Mutex
}

type typingSession struct {
	pending int
	cancel  context.CancelFunc
}

func NewDiscordChannel(cfg config.DiscordConfig, bus *bus.MessageBus) (*DiscordChannel, error) {
	session, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("failed to create discord session: %w", err)
	}

	base := NewBaseChannel("discord", cfg, bus, cfg.AllowFrom)

	return &DiscordChannel{
		BaseChannel: base,
		session:     session,
		config:      cfg,
		typing:      make(map[string]*typingSession),
	}, nil
}

func (c *DiscordChannel) Start(ctx context.Context) error {
	logger.InfoC("discord", "Starting Discord bot")

	c.session.AddHandler(c.handleMessage)

	if err := c.session.Open(); err != nil {
		return fmt.Errorf("failed to open discord session: %w", err)
	}

	c.setRunning(true)

	botUser, err := c.session.User("@me")
	if err != nil {
		return fmt.Errorf("failed to get bot user: %w", err)
	}
	logger.InfoCF("discord", "Discord bot connected", map[string]any{
		"username": botUser.Username,
		"user_id":  botUser.ID,
	})

	return nil
}

func (c *DiscordChannel) Stop(ctx context.Context) error {
	logger.InfoC("discord", "Stopping Discord bot")
	c.setRunning(false)
	c.stopAllTyping()

	if err := c.session.Close(); err != nil {
		return fmt.Errorf("failed to close discord session: %w", err)
	}

	return nil
}

func (c *DiscordChannel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if !c.IsRunning() {
		return fmt.Errorf("discord bot not running")
	}

	channelID := msg.ChatID
	if channelID == "" {
		return fmt.Errorf("channel ID is empty")
	}
	defer c.endTyping(channelID)

	runes := []rune(msg.Content)
	if len(runes) == 0 {
		return nil
	}

	chunks := splitMessage(msg.Content, 1500) // Discord has a limit of 2000 characters per message, leave 500 for natural split e.g. code blocks

	for _, chunk := range chunks {
		if err := c.sendChunk(ctx, channelID, chunk); err != nil {
			return err
		}
	}

	return nil
}

// splitMessage splits long messages into chunks, preserving code block integrity
// Uses natural boundaries (newlines, spaces) and extends messages slightly to avoid breaking code blocks
func splitMessage(content string, limit int) []string {
	var messages []string

	for len(content) > 0 {
		if len(content) <= limit {
			messages = append(messages, content)
			break
		}

		msgEnd := limit

		// Find natural split point within the limit
		msgEnd = findLastNewline(content[:limit], 200)
		if msgEnd <= 0 {
			msgEnd = findLastSpace(content[:limit], 100)
		}
		if msgEnd <= 0 {
			msgEnd = limit
		}

		// Check if this would end with an incomplete code block
		candidate := content[:msgEnd]
		unclosedIdx := findLastUnclosedCodeBlock(candidate)

		if unclosedIdx >= 0 {
			// Message would end with incomplete code block
			// Try to extend to include the closing ``` (with some buffer)
			extendedLimit := limit + 500 // Allow 500 char buffer for code blocks
			if len(content) > extendedLimit {
				closingIdx := findNextClosingCodeBlock(content, msgEnd)
				if closingIdx > 0 && closingIdx <= extendedLimit {
					// Extend to include the closing ```
					msgEnd = closingIdx
				} else {
					// Can't find closing, split before the code block
					msgEnd = findLastNewline(content[:unclosedIdx], 200)
					if msgEnd <= 0 {
						msgEnd = findLastSpace(content[:unclosedIdx], 100)
					}
					if msgEnd <= 0 {
						msgEnd = unclosedIdx
					}
				}
			} else {
				// Remaining content fits within extended limit
				msgEnd = len(content)
			}
		}

		if msgEnd <= 0 {
			msgEnd = limit
		}

		messages = append(messages, content[:msgEnd])
		content = strings.TrimSpace(content[msgEnd:])
	}

	return messages
}

// findLastUnclosedCodeBlock finds the last opening ``` that doesn't have a closing ```
// Returns the position of the opening ``` or -1 if all code blocks are complete
func findLastUnclosedCodeBlock(text string) int {
	count := 0
	lastOpenIdx := -1

	for i := 0; i < len(text); i++ {
		if i+2 < len(text) && text[i] == '`' && text[i+1] == '`' && text[i+2] == '`' {
			if count == 0 {
				lastOpenIdx = i
			}
			count++
			i += 2
		}
	}

	// If odd number of ``` markers, last one is unclosed
	if count%2 == 1 {
		return lastOpenIdx
	}
	return -1
}

// findNextClosingCodeBlock finds the next closing ``` starting from a position
// Returns the position after the closing ``` or -1 if not found
func findNextClosingCodeBlock(text string, startIdx int) int {
	for i := startIdx; i < len(text); i++ {
		if i+2 < len(text) && text[i] == '`' && text[i+1] == '`' && text[i+2] == '`' {
			return i + 3
		}
	}
	return -1
}

// findLastNewline finds the last newline character within the last N characters
// Returns the position of the newline or -1 if not found
func findLastNewline(s string, searchWindow int) int {
	searchStart := len(s) - searchWindow
	if searchStart < 0 {
		searchStart = 0
	}
	for i := len(s) - 1; i >= searchStart; i-- {
		if s[i] == '\n' {
			return i
		}
	}
	return -1
}

// findLastSpace finds the last space character within the last N characters
// Returns the position of the space or -1 if not found
func findLastSpace(s string, searchWindow int) int {
	searchStart := len(s) - searchWindow
	if searchStart < 0 {
		searchStart = 0
	}
	for i := len(s) - 1; i >= searchStart; i-- {
		if s[i] == ' ' || s[i] == '\t' {
			return i
		}
	}
	return -1
}

func (c *DiscordChannel) sendChunk(ctx context.Context, channelID, content string) error {
	// Use the incoming context for timeout control.
	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.session.ChannelMessageSend(channelID, content)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("failed to send discord message: %w", err)
		}
		return nil
	case <-sendCtx.Done():
		return fmt.Errorf("send message timeout: %w", sendCtx.Err())
	}
}

func (c *DiscordChannel) sendTyping(channelID string) {
	if channelID == "" || c.session == nil {
		return
	}
	if err := c.session.ChannelTyping(channelID); err != nil {
		logger.ErrorCF("discord", "Failed to send typing indicator", map[string]any{
			"error": err.Error(),
		})
	}
}

func (c *DiscordChannel) beginTyping(channelID string) {
	if channelID == "" {
		return
	}

	c.typingMu.Lock()
	if sess, ok := c.typing[channelID]; ok {
		sess.pending++
		c.typingMu.Unlock()
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.typing[channelID] = &typingSession{
		pending: 1,
		cancel:  cancel,
	}
	c.typingMu.Unlock()

	c.sendTyping(channelID)

	go func() {
		ticker := time.NewTicker(typingRefreshInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !c.IsRunning() {
					return
				}
				c.sendTyping(channelID)
			}
		}
	}()
}

func (c *DiscordChannel) endTyping(channelID string) {
	if channelID == "" {
		return
	}

	c.typingMu.Lock()
	defer c.typingMu.Unlock()

	sess, ok := c.typing[channelID]
	if !ok {
		return
	}
	sess.pending--
	if sess.pending > 0 {
		return
	}
	delete(c.typing, channelID)
	sess.cancel()
}

func (c *DiscordChannel) stopAllTyping() {
	c.typingMu.Lock()
	defer c.typingMu.Unlock()

	for channelID, sess := range c.typing {
		sess.cancel()
		delete(c.typing, channelID)
	}
}

// appendContent safely appends suffix text to existing content.
func appendContent(content, suffix string) string {
	if content == "" {
		return suffix
	}
	return content + "\n" + suffix
}

func (c *DiscordChannel) handleMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m == nil || m.Author == nil {
		return
	}

	if m.Author.ID == s.State.User.ID {
		return
	}

	// Check allowlist before downloading attachments or transcribing audio.
	if !c.IsAllowed(m.Author.ID) {
		logger.DebugCF("discord", "Message rejected by allowlist", map[string]any{
			"user_id": m.Author.ID,
		})
		return
	}

	senderID := m.Author.ID
	senderName := m.Author.Username
	if m.Author.Discriminator != "" && m.Author.Discriminator != "0" {
		senderName += "#" + m.Author.Discriminator
	}

	content := m.Content
	mediaPaths := make([]string, 0, len(m.Attachments))

	for _, attachment := range m.Attachments {
		isAudio := utils.IsAudioFile(attachment.Filename, attachment.ContentType)

		if isAudio {
			mediaPaths = append(mediaPaths, attachment.URL)
			content = appendContent(content, fmt.Sprintf("[audio: %s]", attachment.Filename))
		} else {
			mediaPaths = append(mediaPaths, attachment.URL)
			content = appendContent(content, fmt.Sprintf("[attachment: %s]", attachment.URL))
		}
	}

	if content == "" && len(mediaPaths) == 0 {
		return
	}

	if content == "" {
		content = "[media only]"
	}

	c.beginTyping(m.ChannelID)

	logger.DebugCF("discord", "Received message", map[string]any{
		"sender_name": senderName,
		"sender_id":   senderID,
		"preview":     utils.Truncate(content, 50),
	})

	metadata := map[string]string{
		"message_id":   m.ID,
		"user_id":      senderID,
		"username":     m.Author.Username,
		"display_name": senderName,
		"guild_id":     m.GuildID,
		"channel_id":   m.ChannelID,
		"is_dm":        fmt.Sprintf("%t", m.GuildID == ""),
	}

	c.HandleMessage(senderID, m.ChannelID, content, mediaPaths, metadata)
}
