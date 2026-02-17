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
