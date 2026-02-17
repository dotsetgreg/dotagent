package agent

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
)

const sessionKeyVersion = "v2"

type SessionIdentity struct {
	WorkspaceID    string
	Channel        string
	ConversationID string
	ActorID        string
}

func (id SessionIdentity) Validate() error {
	if strings.TrimSpace(id.WorkspaceID) == "" {
		return fmt.Errorf("missing workspace id")
	}
	if strings.TrimSpace(id.Channel) == "" {
		return fmt.Errorf("missing channel")
	}
	if strings.TrimSpace(id.ConversationID) == "" {
		return fmt.Errorf("missing conversation id")
	}
	if strings.TrimSpace(id.ActorID) == "" {
		return fmt.Errorf("missing actor id")
	}
	return nil
}

func (id SessionIdentity) Canonical() string {
	return strings.ToLower(strings.TrimSpace(id.WorkspaceID)) + "|" +
		strings.ToLower(strings.TrimSpace(id.Channel)) + "|" +
		strings.TrimSpace(id.ConversationID) + "|" +
		strings.TrimSpace(id.ActorID)
}

func (id SessionIdentity) SessionKey() string {
	payload := id.Canonical()
	sum := sha1.Sum([]byte(payload))
	return sessionKeyVersion + ":" + hex.EncodeToString(sum[:16])
}

func workspaceNamespace(workspacePath string) string {
	ws := strings.TrimSpace(strings.ToLower(workspacePath))
	if ws == "" {
		ws = "default-workspace"
	}
	sum := sha1.Sum([]byte(ws))
	return "ws-" + hex.EncodeToString(sum[:8])
}

func isV2SessionKey(sessionKey string) bool {
	return strings.HasPrefix(strings.TrimSpace(sessionKey), sessionKeyVersion+":")
}

func resolveSessionKey(explicitKey, workspaceID, channel, conversationID, actorID string) (string, error) {
	explicitKey = strings.TrimSpace(explicitKey)
	if isV2SessionKey(explicitKey) {
		return explicitKey, nil
	}
	identity := SessionIdentity{
		WorkspaceID:    strings.TrimSpace(workspaceID),
		Channel:        strings.TrimSpace(channel),
		ConversationID: strings.TrimSpace(conversationID),
		ActorID:        strings.TrimSpace(actorID),
	}
	if err := identity.Validate(); err != nil {
		if explicitKey != "" {
			// Legacy fallback for strict backward compatibility.
			return explicitKey, nil
		}
		return "", fmt.Errorf("resolve session identity: %w", err)
	}
	return identity.SessionKey(), nil
}
