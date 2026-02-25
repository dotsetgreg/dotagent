package memory

// DeriveContextBudget allocates context tokens across system/thread/summary/memory.
func DeriveContextBudget(total int) ContextBudget {
	if total <= 0 {
		total = 16384
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

// ScaleContextBudget applies a global safety factor to a budget while preserving
// minimum section sizes and overall proportions.
func ScaleContextBudget(b ContextBudget, factor float64) ContextBudget {
	if factor <= 0 || factor >= 1 {
		return b
	}
	total := int(float64(b.TotalTokens) * factor)
	if total < 1024 {
		total = 1024
	}

	scalePart := func(v, min int) int {
		if v <= 0 {
			return min
		}
		scaled := int(float64(v) * factor)
		if scaled < min {
			return min
		}
		return scaled
	}

	system := scalePart(b.SystemTokens, 192)
	thread := scalePart(b.ThreadTokens, 320)
	memory := scalePart(b.MemoryTokens, 256)
	summary := scalePart(b.SummaryTokens, 96)

	parts := []*int{&thread, &system, &memory, &summary}
	sum := system + thread + memory + summary
	for sum > total {
		reduced := false
		for _, part := range parts {
			switch part {
			case &thread:
				if *part > 320 {
					*part -= minInt(64, *part-320)
					reduced = true
				}
			case &system:
				if *part > 192 {
					*part -= minInt(48, *part-192)
					reduced = true
				}
			case &memory:
				if *part > 256 {
					*part -= minInt(48, *part-256)
					reduced = true
				}
			case &summary:
				if *part > 96 {
					*part -= minInt(32, *part-96)
					reduced = true
				}
			}
			if reduced {
				break
			}
		}
		if !reduced {
			break
		}
		sum = system + thread + memory + summary
	}
	if sum < total {
		thread += total - sum
	}

	return ContextBudget{
		TotalTokens:   total,
		SystemTokens:  system,
		ThreadTokens:  thread,
		MemoryTokens:  memory,
		SummaryTokens: summary,
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
