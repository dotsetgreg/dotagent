package agent

import "testing"

func TestResolveSessionKey_DeterministicV2(t *testing.T) {
	workspaceID := workspaceNamespace("/tmp/workspace")
	k1, err := resolveSessionKey("", workspaceID, "discord", "chat-1", "user-1")
	if err != nil {
		t.Fatalf("resolve session key: %v", err)
	}
	k2, err := resolveSessionKey("", workspaceID, "discord", "chat-1", "user-1")
	if err != nil {
		t.Fatalf("resolve session key second call: %v", err)
	}
	if k1 != k2 {
		t.Fatalf("expected deterministic session keys, got %q vs %q", k1, k2)
	}
	if !isV2SessionKey(k1) {
		t.Fatalf("expected v2 session key, got %q", k1)
	}
}

func TestResolveSessionKey_DiffersByActor(t *testing.T) {
	workspaceID := workspaceNamespace("/tmp/workspace")
	k1, err := resolveSessionKey("", workspaceID, "discord", "chat-1", "user-a")
	if err != nil {
		t.Fatalf("resolve session key actor A: %v", err)
	}
	k2, err := resolveSessionKey("", workspaceID, "discord", "chat-1", "user-b")
	if err != nil {
		t.Fatalf("resolve session key actor B: %v", err)
	}
	if k1 == k2 {
		t.Fatalf("expected different keys for different actors")
	}
}

func TestResolveSessionKey_LegacyFallback(t *testing.T) {
	got, err := resolveSessionKey("legacy:key", "", "", "", "")
	if err != nil {
		t.Fatalf("resolve legacy session key: %v", err)
	}
	if got != "legacy:key" {
		t.Fatalf("expected legacy fallback, got %q", got)
	}
}
