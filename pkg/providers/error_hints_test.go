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

func TestAugmentProviderError_OpenAICodexCloudflareHint(t *testing.T) {
	msg := augmentProviderError(ProviderOpenAICodex, "Just a moment... Enable JavaScript and cookies to continue")
	if !strings.Contains(msg, "chatgpt.com/backend-api") {
		t.Fatalf("expected chatgpt backend hint, got %q", msg)
	}
}

func TestAugmentProviderError_OpenAICodexClaimHint(t *testing.T) {
	msg := augmentProviderError(ProviderOpenAICodex, "missing chatgpt_account_id in OpenAI Codex token")
	if !strings.Contains(msg, "chatgpt_account_id") {
		t.Fatalf("expected account-id hint, got %q", msg)
	}
}

func TestAugmentProviderError_OpenAICodexMaxOutputTokensHint(t *testing.T) {
	msg := augmentProviderError(ProviderOpenAICodex, "Unsupported parameter: max_output_tokens")
	if !strings.Contains(strings.ToLower(msg), "max_output_tokens") {
		t.Fatalf("expected max_output_tokens hint, got %q", msg)
	}
}

func TestAugmentProviderError_OpenAICodexTemperatureHint(t *testing.T) {
	msg := augmentProviderError(ProviderOpenAICodex, "Unsupported parameter: temperature")
	if !strings.Contains(strings.ToLower(msg), "temperature") {
		t.Fatalf("expected temperature hint, got %q", msg)
	}
}
