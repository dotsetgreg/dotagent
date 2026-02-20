# dotagent Architecture Review & Plan

## 1. DRY / Consolidation (Robustness & Cleanliness)
- `runLLMIteration` (in `pkg/agent/loop.go`) and `RunToolLoop` (in `pkg/tools/toolloop.go`) are almost identical duplicates. They both handle the core LLM execution, retries, and tool unpacking.
  - **Issue:** `RunToolLoop` is missing the 3-loop circuit breaker logic that `runLLMIteration` has. It's also missing the logic for the `maxIterations` pause message that we just added. This means subagents using `RunToolLoop` are prone to infinite loops.
  - **Fix:** Consolidate these into a single robust engine. `RunToolLoop` should be the only implementation, and the main AgentLoop should just call it and then handle the database/memory tracking for its own turn events.


## 2. LLM Context Retries / Force Compaction (Robustness)
- In `runLLMIteration`, when a context window error occurs (e.g. "token", "invalid", "length" in the error message string), the agent calls `al.memory.ForceCompact` and retries the prompt. 
  - **Issue:** The logic to detect context errors is purely string-matching on the error message (`strings.Contains(errMsg, "token") || strings.Contains(errMsg, "context")`). Different providers have completely different, sometimes vague error messages for context exhaustion.
  - **Issue:** Subagents using `RunToolLoop` do not have any token compaction or retry logic, meaning subagents will hard-crash if they read a large file or run too long.
  - **Fix:** Standardize error types across providers (e.g., `providers.ErrContextExceeded`). And, pull the retry/compaction logic into the shared ToolLoop so subagents can also self-heal from context exhaustion.


## 3. Persona / Memory Ingestion Overhead (Efficiency & Optimization)
- In the main agent loop, the agent has a synchronous loop running on every single user turn to extract Persona Candidates (`al.memory.ApplyPersonaDirectivesSync`). This actually makes a *secondary* LLM call behind the scenes (`personaExtractFn`) *before* the agent is even allowed to start thinking about the user's prompt. 
  - **Issue:** This blocks the user from getting a fast response because they have to wait for the LLM to process the entire persona extraction loop before the main agent even begins running tools.
  - **Fix:** If the user hasn't explicitly used a phrase that implies a durable persona change (e.g. "call me X", "always do Y"), we should skip the synchronous persona extraction block entirely, OR run it asynchronously and lazily apply it to the *next* turn.
