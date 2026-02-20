package memory

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// HeuristicConsolidator extracts durable memories from turns.
type HeuristicConsolidator struct {
	store  Store
	policy Policy
}

func NewHeuristicConsolidator(store Store, policy Policy) *HeuristicConsolidator {
	return &HeuristicConsolidator{store: store, policy: policy}
}

func (c *HeuristicConsolidator) ConsolidateTurn(ctx context.Context, sessionKey, turnID, userID, agentID string) error {
	turnEvents, err := c.store.ListEventsByTurn(ctx, sessionKey, turnID, 64)
	if err != nil {
		return err
	}
	if len(turnEvents) == 0 {
		return nil
	}

	ttlFor := func(kind MemoryItemKind, override time.Duration) int64 {
		if override > 0 {
			return time.Now().Add(override).UnixMilli()
		}
		if c.policy == nil {
			return 0
		}
		return c.policy.TTLFor(kind)
	}

	ops := []ConsolidationOp{}
	for _, ev := range turnEvents {
		if c.policy != nil && !c.policy.ShouldCapture(ev) {
			continue
		}
		content := strings.TrimSpace(ev.Content)
		if content == "" {
			continue
		}

		if ev.Role == "user" {
			for _, m := range forgetRegex.FindAllStringSubmatch(content, -1) {
				if len(m) < 2 {
					continue
				}
				term := strings.TrimSpace(m[1])
				if term == "" {
					continue
				}
				candidates, err := c.store.SearchMemoryFTS(ctx, userID, agentID, sessionKey, buildFTSQuery(term), 5)
				if err == nil {
					for _, cand := range candidates {
						ops = append(ops, ConsolidationOp{
							Action:     "delete",
							Kind:       cand.Kind,
							Key:        cand.Key,
							Content:    cand.Content,
							Confidence: 1.0,
							Metadata:   map[string]string{"source_role": ev.Role, "reason": "user_forget_request"},
						})
					}
				}
			}
			ops = append(ops, extractUserContentUpsertOps(content, ev.ID)...)
		}
	}

	// Always add a compact episodic turn note.
	episodic := summarizeTurn(turnEvents)
	if episodic != "" {
		ops = append(ops, ConsolidationOp{
			Action:     "upsert",
			Kind:       MemoryEpisodic,
			Key:        contentKey("turn", turnID+":"+episodic),
			Content:    episodic,
			Confidence: 0.6,
			TTL:        30 * 24 * time.Hour,
			Metadata:   map[string]string{"turn_id": turnID},
		})
	}

	if len(ops) == 0 {
		_ = c.store.MarkSessionConsolidated(ctx, sessionKey, time.Now().UnixMilli())
		return nil
	}

	inserted := []MemoryItem{}
	for _, op := range ops {
		if op.Action == "delete" {
			if err := c.store.DeleteMemoryByKey(ctx, userID, agentID, op.Kind, op.Key); err != nil {
				return err
			}
			continue
		}

		if c.policy != nil && op.Confidence < c.policy.MinConfidence(op.Kind) {
			continue
		}
		scopeType, scopeID := deriveScopeForOp(op.Kind, sessionKey, userID, op.Metadata)

		item, err := c.store.UpsertMemoryItem(ctx, MemoryItem{
			ID:            "mem-" + uuid.NewString(),
			UserID:        userID,
			AgentID:       agentID,
			ScopeType:     scopeType,
			ScopeID:       scopeID,
			SessionKey:    sessionKey,
			Kind:          op.Kind,
			Key:           op.Key,
			Content:       strings.TrimSpace(op.Content),
			Confidence:    op.Confidence,
			Weight:        1,
			SourceEventID: op.SourceEvent,
			FirstSeenAtMS: time.Now().UnixMilli(),
			LastSeenAtMS:  time.Now().UnixMilli(),
			ExpiresAtMS:   ttlFor(op.Kind, op.TTL),
			Metadata:      op.Metadata,
		})
		if err != nil {
			return err
		}
		vec := embedText(item.Content)
		if err := c.store.UpsertEmbedding(ctx, item.ID, currentEmbeddingModel(), vec); err != nil {
			return err
		}
		inserted = append(inserted, item)
	}

	for i := 0; i < len(inserted)-1; i++ {
		left := inserted[i]
		right := inserted[i+1]
		if left.ID == right.ID {
			continue
		}
		_ = c.store.UpsertMemoryLink(ctx, MemoryLink{
			ID:          "lnk-" + uuid.NewString(),
			FromItemID:  left.ID,
			ToItemID:    right.ID,
			Relation:    "cooccurred_turn",
			Weight:      0.5,
			CreatedAtMS: time.Now().UnixMilli(),
		})
	}

	_ = c.store.AddMetric(ctx, "memory.consolidation.items", float64(len(inserted)), map[string]string{"session_key": sessionKey})
	if err := c.store.MarkSessionConsolidated(ctx, sessionKey, time.Now().UnixMilli()); err != nil {
		return err
	}
	return nil
}

func summarizeTurn(events []Event) string {
	if len(events) == 0 {
		return ""
	}
	user := ""
	assistant := ""
	for _, ev := range events {
		snippet := strings.TrimSpace(ev.Content)
		if len(snippet) > 220 {
			snippet = snippet[:220] + "..."
		}
		switch ev.Role {
		case "user":
			if user == "" {
				user = snippet
			}
		case "assistant":
			if assistant == "" {
				assistant = snippet
			}
		}
	}
	if user == "" && assistant == "" {
		return ""
	}
	if assistant == "" {
		return fmt.Sprintf("Turn recap: User said %q", user)
	}
	if user == "" {
		return fmt.Sprintf("Turn recap: Assistant responded %q", assistant)
	}
	return fmt.Sprintf("Turn recap: User asked %q; assistant responded %q", user, assistant)
}

func contentKey(prefix, content string) string {
	n := strings.ToLower(strings.TrimSpace(content))
	h := sha1.Sum([]byte(prefix + ":" + n))
	return prefix + "/" + hex.EncodeToString(h[:8])
}
