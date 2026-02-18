package providers

import (
	"strings"
	"testing"
)

func TestAugmentProviderError_OpenAIScopeHint(t *testing.T) {
	msg := augmentProviderError(ProviderOpenAI, "You have insufficient permissions for this operation. Missing scopes: model.request.")
	if !strings.Contains(msg, "openai-codex") {
		t.Fatalf("expected openai-codex guidance in hint, got %q", msg)
	}
}

func TestAugmentProviderError_OpenAIIncorrectAPIKeyHint(t *testing.T) {
	msg := augmentProviderError(ProviderOpenAI, "Incorrect API key provided")
	if !strings.Contains(msg, "Platform API credential") {
		t.Fatalf("expected platform credential hint, got %q", msg)
	}
}
