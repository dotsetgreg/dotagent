package memory

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	prefRegex      = regexp.MustCompile(`(?i)\b(i (?:really )?(?:like|love|prefer|hate|dislike)\b[^.!?\n]*)`)
	identityRegex  = regexp.MustCompile(`(?i)\b(?:my name is|i am|i'm)\s+([A-Za-z0-9 _\-]{2,50})`)
	timezoneRegex  = regexp.MustCompile(`(?i)\b(?:my timezone is|i am in|i'm in|i live in)\s+([A-Za-z0-9_\-/:+ ]{2,80})`)
	taskStateRegex = regexp.MustCompile(`(?i)\b(remind me|schedule|todo|task|deadline)\b([^.!?\n]*)`)
	forgetRegex    = regexp.MustCompile(`(?i)\bforget(?: that| this| about)?\s+(.+)$`)
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
	events, err := c.store.ListRecentEvents(ctx, sessionKey, 64, false)
	if err != nil {
		return err
	}

	turnEvents := make([]Event, 0, 8)
	for _, ev := range events {
		if ev.TurnID == turnID {
			turnEvents = append(turnEvents, ev)
		}
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

			for _, m := range prefRegex.FindAllStringSubmatch(content, -1) {
				if len(m) < 2 {
					continue
				}
				entry := strings.TrimSpace(m[1])
				ops = append(ops, ConsolidationOp{
					Action:      "upsert",
					Kind:        MemoryUserPreference,
					Key:         contentKey("pref", entry),
					Content:     entry,
					Confidence:  0.8,
					SourceEvent: ev.ID,
					Metadata:    map[string]string{"source_role": ev.Role},
				})
			}

			for _, m := range identityRegex.FindAllStringSubmatch(content, -1) {
				if len(m) < 2 {
					continue
				}
				identity := strings.TrimSpace(m[1])
				if len(identity) < 2 {
					continue
				}
				ops = append(ops, ConsolidationOp{
					Action:      "upsert",
					Kind:        MemorySemanticFact,
					Key:         "identity/name",
					Content:     "User identity hint: " + identity,
					Confidence:  0.75,
					SourceEvent: ev.ID,
					Metadata:    map[string]string{"source_role": ev.Role},
				})
			}

			for _, m := range timezoneRegex.FindAllStringSubmatch(content, -1) {
				if len(m) < 2 {
					continue
				}
				tz := strings.TrimSpace(m[1])
				ops = append(ops, ConsolidationOp{
					Action:      "upsert",
					Kind:        MemorySemanticFact,
					Key:         "profile/timezone_or_location",
					Content:     "User timezone/location: " + tz,
					Confidence:  0.7,
					SourceEvent: ev.ID,
					Metadata:    map[string]string{"source_role": ev.Role},
				})
			}

			for _, m := range taskStateRegex.FindAllStringSubmatch(content, -1) {
				if len(m) < 2 {
					continue
				}
				task := strings.TrimSpace(strings.Join(m[1:], " "))
				ops = append(ops, ConsolidationOp{
					Action:      "upsert",
					Kind:        MemoryTaskState,
					Key:         contentKey("task", task),
					Content:     "Open task intent: " + task,
					Confidence:  0.6,
					SourceEvent: ev.ID,
					TTL:         14 * 24 * time.Hour,
					Metadata:    map[string]string{"source_role": ev.Role},
				})
			}
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

		item, err := c.store.UpsertMemoryItem(ctx, MemoryItem{
			ID:            "mem-" + uuid.NewString(),
			UserID:        userID,
			AgentID:       agentID,
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
		if err := c.store.UpsertEmbedding(ctx, item.ID, embeddingModel, vec); err != nil {
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
