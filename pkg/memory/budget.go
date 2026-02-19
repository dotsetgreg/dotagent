package memory

// DeriveContextBudget allocates context tokens across system/thread/summary/memory.
func DeriveContextBudget(total int) ContextBudget {
	if total <= 0 {
		total = 8192
	}
	system := total * 25 / 100
	thread := total * 45 / 100
	summary := total * 10 / 100
	memory := total - system - thread - summary
	if memory < 512 {
		memory = 512
		if thread > 1024 {
			thread -= 256
		}
	}
	return ContextBudget{
		TotalTokens:   total,
		SystemTokens:  system,
		ThreadTokens:  thread,
		MemoryTokens:  memory,
		SummaryTokens: summary,
	}
}

type BudgetSignals struct {
	RecentEventCount int
	Query            string
	HasSummary       bool
	HasRecall        bool
}

// DeriveAdaptiveContextBudget adjusts token allocation based on session/query pressure.
func DeriveAdaptiveContextBudget(total int, signals BudgetSignals) ContextBudget {
	b := DeriveContextBudget(total)
	if signals.RecentEventCount > 40 {
		shift := minInt(256, b.MemoryTokens/4)
		b.ThreadTokens += shift
		b.MemoryTokens -= shift
	}
	normalizedQuery, _ := normalizeIntentQuery(signals.Query)
	if containsAnyIntentPhrase(normalizedQuery, []string{"already", "earlier", "before", "as i said", "as i mentioned"}) {
		shift := minInt(384, b.MemoryTokens/3)
		b.ThreadTokens += shift
		b.MemoryTokens -= shift
	}
	if !signals.HasSummary {
		shift := minInt(128, b.SummaryTokens/2)
		b.SummaryTokens -= shift
		b.ThreadTokens += shift
	}
	if !signals.HasRecall {
		shift := minInt(96, b.MemoryTokens/5)
		b.MemoryTokens -= shift
		b.ThreadTokens += shift
	}
	if b.MemoryTokens < 256 {
		diff := 256 - b.MemoryTokens
		if b.ThreadTokens > diff+512 {
			b.ThreadTokens -= diff
			b.MemoryTokens += diff
		}
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
