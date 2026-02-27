package agent

import (
	"regexp"
	"strings"
)

var personaSyncLikelyQuestionLeadRegex = regexp.MustCompile(`(?i)^\s*(?:what|why|how|when|where|who|can|could|would|do|does|did|is|are|am|if|whether)\b`)

var personaSyncStrongCueRegex = regexp.MustCompile(`(?i)\b(?:my name is|call me|your name is|call yourself|you(?:'re| are) called|from now on|forget|remove|my timezone is|i(?:'m| am) in timezone|my preferred language is|my language is|respond in|speak in|set (?:my|your)|(?:please\s+)?remember this[:\s]|for this session|your\s+[a-z][a-z0-9 _\-]{1,32}\s+is)\b`)

var personaSyncStyleCueRegex = regexp.MustCompile(`(?i)\b(?:be|respond|talk|write)\s+(?:more\s+)?(?:concise|detailed|formal|casual|direct|friendly)\b`)

var personaSyncQuestionDirectiveCueRegex = regexp.MustCompile(`(?i)\b(?:call me|call yourself|your name is|my name is|respond in|speak in|for this session|set (?:my|your)|forget|remove)\b`)

// shouldApplyPersonaSyncFastPath decides whether to run synchronous persona mutation
// before response generation. Non-matching turns are handled asynchronously by
// scheduled maintenance.
func shouldApplyPersonaSyncFastPath(userMessage string) bool {
	content := strings.TrimSpace(userMessage)
	if content == "" {
		return false
	}
	if personaSyncLikelyQuestionLeadRegex.MatchString(content) && !personaSyncQuestionDirectiveCueRegex.MatchString(content) {
		return false
	}
	return personaSyncStrongCueRegex.MatchString(content) || personaSyncStyleCueRegex.MatchString(content)
}
