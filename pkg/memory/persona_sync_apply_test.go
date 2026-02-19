package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPersonaApplySync_SecondPersonIdentityImmediate(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	svc, err := NewService(Config{
		Workspace:         ws,
		AgentID:           "dotagent",
		WorkerPoll:        40 * time.Millisecond,
		PersonaSyncApply:  true,
		PersonaFileSync:   PersonaFileSyncExportOnly,
		PersonaPolicyMode: "balanced",
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	sessionKey := "discord:sync-identity"
	userID := "u-sync-identity"
	if err := svc.EnsureSession(ctx, sessionKey, "discord", "sync-identity", userID); err != nil {
		t.Fatalf("ensure session: %v", err)
	}

	turnID := "turn-sync"
	if _, _, err := svc.RecordUserTurn(ctx, Event{
		SessionKey: sessionKey,
		TurnID:     turnID,
		Seq:        1,
		Role:       "user",
		Content:    "Your name is Luna. You're my personal assistant and you have a pony tail.",
	}, userID); err != nil {
		t.Fatalf("record user turn: %v", err)
	}

	report, err := svc.ApplyPersonaDirectivesSync(ctx, sessionKey, turnID, userID)
	if err != nil {
		t.Fatalf("apply persona sync: %v", err)
	}
	if report.AcceptedCount() == 0 {
		t.Fatalf("expected at least one accepted persona mutation, got %+v", report)
	}

	profile, err := svc.GetPersonaProfile(ctx, userID)
	if err != nil {
		t.Fatalf("get persona profile: %v", err)
	}
	if !strings.EqualFold(profile.Identity.AgentName, "Luna") {
		t.Fatalf("expected agent name Luna, got %q", profile.Identity.AgentName)
	}
	if !strings.Contains(strings.ToLower(profile.Identity.Role), "assistant") {
		t.Fatalf("expected assistant role, got %q", profile.Identity.Role)
	}
	if len(profile.Identity.Attributes) == 0 {
		t.Fatalf("expected identity attributes from second-person directives")
	}

	pc, err := svc.BuildPromptContext(ctx, sessionKey, userID, "what is your name", 2048)
	if err != nil {
		t.Fatalf("build prompt context: %v", err)
	}
	if !strings.Contains(strings.ToLower(pc.PersonaPrompt), "agent name: luna") {
		t.Fatalf("expected persona prompt to include Luna, got: %s", pc.PersonaPrompt)
	}
}

func TestPersonaFileSyncExportOnly_DoesNotImportWorkspaceFiles(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	svc, err := NewService(Config{
		Workspace:       ws,
		AgentID:         "dotagent",
		WorkerPoll:      40 * time.Millisecond,
		PersonaFileSync: PersonaFileSyncExportOnly,
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	userID := "u-export-only"
	session := "discord:export-only"
	_ = svc.EnsureSession(ctx, session, "discord", "export-only", userID)

	userFile := `# User

## Name
Jordan

## Timezone
Europe/Berlin
`
	if err := os.WriteFile(filepath.Join(ws, "USER.md"), []byte(userFile), 0o644); err != nil {
		t.Fatalf("write USER.md: %v", err)
	}

	if _, err := svc.BuildPromptContext(ctx, session, userID, "profile check", 2048); err != nil {
		t.Fatalf("build prompt context: %v", err)
	}

	profile, err := svc.GetPersonaProfile(ctx, userID)
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if strings.EqualFold(profile.User.Name, "Jordan") {
		t.Fatalf("expected export-only mode to avoid reverse-import from USER.md")
	}
}

func TestPersonaSyncApply_IsIdempotentPerTurn(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	svc, err := NewService(Config{
		Workspace:        ws,
		AgentID:          "dotagent",
		WorkerPoll:       40 * time.Millisecond,
		PersonaSyncApply: true,
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	sessionKey := "discord:idempotent"
	userID := "u-idempotent"
	if err := svc.EnsureSession(ctx, sessionKey, "discord", "idempotent", userID); err != nil {
		t.Fatalf("ensure session: %v", err)
	}

	turnID := "turn-idempotent"
	if _, _, err := svc.RecordUserTurn(ctx, Event{
		SessionKey: sessionKey,
		TurnID:     turnID,
		Seq:        1,
		Role:       "user",
		Content:    "Your name is Luna.",
	}, userID); err != nil {
		t.Fatalf("record user turn: %v", err)
	}

	first, err := svc.ApplyPersonaDirectivesSync(ctx, sessionKey, turnID, userID)
	if err != nil {
		t.Fatalf("first sync apply: %v", err)
	}
	second, err := svc.ApplyPersonaDirectivesSync(ctx, sessionKey, turnID, userID)
	if err != nil {
		t.Fatalf("second sync apply: %v", err)
	}
	if first.AcceptedCount() == 0 {
		t.Fatalf("expected first apply to accept mutation")
	}
	if second.AcceptedCount() > 0 {
		t.Fatalf("expected second apply to be idempotent, got %+v", second)
	}

	revs, err := svc.ListPersonaRevisions(ctx, userID, 20)
	if err != nil {
		t.Fatalf("list persona revisions: %v", err)
	}
	count := 0
	for _, rev := range revs {
		if rev.FieldPath == "identity.agent_name" && strings.EqualFold(rev.NewValue, "Luna") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected one identity.agent_name revision for Luna, got %d", count)
	}
}

func TestPersonaSyncApply_QuestionDoesNotMutateName(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	svc, err := NewService(Config{
		Workspace:        ws,
		AgentID:          "dotagent",
		WorkerPoll:       40 * time.Millisecond,
		PersonaSyncApply: true,
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	sessionKey := "discord:question-name"
	userID := "u-question-name"
	if err := svc.EnsureSession(ctx, sessionKey, "discord", "question-name", userID); err != nil {
		t.Fatalf("ensure session: %v", err)
	}

	turnID := "turn-question"
	if _, _, err := svc.RecordUserTurn(ctx, Event{
		SessionKey: sessionKey,
		TurnID:     turnID,
		Seq:        1,
		Role:       "user",
		Content:    "What if my name is Bob?",
	}, userID); err != nil {
		t.Fatalf("record user turn: %v", err)
	}

	if _, err := svc.ApplyPersonaDirectivesSync(ctx, sessionKey, turnID, userID); err != nil {
		t.Fatalf("apply persona sync: %v", err)
	}

	profile, err := svc.GetPersonaProfile(ctx, userID)
	if err != nil {
		t.Fatalf("get persona profile: %v", err)
	}
	if strings.EqualFold(profile.User.Name, "Bob") {
		t.Fatalf("expected question-form text to avoid mutating user.name")
	}
}

func TestPersonaSyncApply_RespondInDetailDoesNotSetLanguage(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	svc, err := NewService(Config{
		Workspace:        ws,
		AgentID:          "dotagent",
		WorkerPoll:       40 * time.Millisecond,
		PersonaSyncApply: true,
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	sessionKey := "discord:style-not-language"
	userID := "u-style-not-language"
	if err := svc.EnsureSession(ctx, sessionKey, "discord", "style-not-language", userID); err != nil {
		t.Fatalf("ensure session: %v", err)
	}

	turnID := "turn-style"
	if _, _, err := svc.RecordUserTurn(ctx, Event{
		SessionKey: sessionKey,
		TurnID:     turnID,
		Seq:        1,
		Role:       "user",
		Content:    "Please respond in detailed format.",
	}, userID); err != nil {
		t.Fatalf("record user turn: %v", err)
	}

	if _, err := svc.ApplyPersonaDirectivesSync(ctx, sessionKey, turnID, userID); err != nil {
		t.Fatalf("apply persona sync: %v", err)
	}

	profile, err := svc.GetPersonaProfile(ctx, userID)
	if err != nil {
		t.Fatalf("get persona profile: %v", err)
	}
	if strings.TrimSpace(profile.User.Language) != "" {
		t.Fatalf("expected non-language directive to avoid mutating user.language, got %q", profile.User.Language)
	}
}

func TestPersonaSyncApply_IWantToDoesNotCreateDurableGoal(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	svc, err := NewService(Config{
		Workspace:        ws,
		AgentID:          "dotagent",
		WorkerPoll:       40 * time.Millisecond,
		PersonaSyncApply: true,
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.Close()

	sessionKey := "discord:no-goal-from-request"
	userID := "u-no-goal-from-request"
	if err := svc.EnsureSession(ctx, sessionKey, "discord", "no-goal-from-request", userID); err != nil {
		t.Fatalf("ensure session: %v", err)
	}

	turnID := "turn-request-goal"
	if _, _, err := svc.RecordUserTurn(ctx, Event{
		SessionKey: sessionKey,
		TurnID:     turnID,
		Seq:        1,
		Role:       "user",
		Content:    "I want to debug this issue right now.",
	}, userID); err != nil {
		t.Fatalf("record user turn: %v", err)
	}

	if _, err := svc.ApplyPersonaDirectivesSync(ctx, sessionKey, turnID, userID); err != nil {
		t.Fatalf("apply persona sync: %v", err)
	}

	profile, err := svc.GetPersonaProfile(ctx, userID)
	if err != nil {
		t.Fatalf("get persona profile: %v", err)
	}
	if len(profile.User.Goals) > 0 {
		t.Fatalf("expected ephemeral request to avoid mutating user.goals, got %+v", profile.User.Goals)
	}
}
