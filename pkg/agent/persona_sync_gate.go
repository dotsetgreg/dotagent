package agent

import (
	"regexp"
	"strings"
)

var personaSyncLikelyQuestionLeadRegex = regexp.MustCompile(`(?i)^\s*(?:what|why|how|when|where|who|can|could|would|do|does|did|is|are|am|if|whether)\b`)

var personaSyncStrongCueRegex = regexp.MustCompile(`(?i)\b(?:my name is|call me|your name is|call yourself|you(?:'re| are) called|from now on|always|never|forget|remove|my timezone is|i(?:'m| am) in timezone|i(?:'m| am) based in|i live in|my location is|my preferred language is|my language is|respond in|speak in|you should|your\s+[a-z][a-z0-9 _\-]{1,32}\s+is)\b`)

var personaSyncStyleCueRegex = regexp.MustCompile(`(?i)\b(?:be|respond|talk|write)\s+(?:more\s+)?(?:concise|detailed|formal|casual|direct|friendly)\b`)

var personaSyncWeakCueRegex = regexp.MustCompile(`(?i)\b(?:please remember|remember this|remember that|note that|save this|save that|store this|store that|my goal is|my goals are|one of my goals is|for this session|i (?:really )?(?:like|love|prefer|hate|dislike))\b`)

// shouldApplyPersonaSyncFastPath decides whether to run synchronous persona mutation
// before response generation. Non-matching turns are handled asynchronously by
// scheduled maintenance.
func shouldApplyPersonaSyncFastPath(userMessage string) bool {
	content := strings.TrimSpace(userMessage)
	if content == "" {
		return false
	}

	if personaSyncStrongCueRegex.MatchString(content) || personaSyncStyleCueRegex.MatchString(content) {
		return true
	}
	if !personaSyncWeakCueRegex.MatchString(content) {
		return false
	}
	if personaSyncLikelyQuestionLeadRegex.MatchString(content) {
		return false
	}
	return true
}
