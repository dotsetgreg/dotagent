package providers

import "strings"

func augmentProviderError(providerName, message string) string {
	msg := strings.TrimSpace(message)
	if msg == "" {
		return msg
	}

	lower := strings.ToLower(msg)
	providerName = NormalizeProviderName(providerName)

	switch providerName {
	case ProviderOpenAI:
		if strings.Contains(lower, "missing scopes: model.request") ||
			strings.Contains(lower, "insufficient permissions for this operation") {
			return msg + " Hint: OpenAI API calls require model.request access. If you are using a ChatGPT/Codex OAuth token, configure provider openai-codex instead."
		}
		if strings.Contains(lower, "incorrect api key provided") {
			return msg + " Hint: provider openai expects a Platform API credential. For ChatGPT/Codex OAuth, use provider openai-codex."
		}
	case ProviderOpenAICodex:
		if strings.Contains(lower, "missing scopes: model.request") ||
			strings.Contains(lower, "insufficient permissions for this operation") {
			return msg + " Hint: your OAuth token does not currently have model.request scope for this account/project."
		}
	}

	return msg
}
