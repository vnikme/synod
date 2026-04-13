package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"cloud.google.com/go/firestore"
)

// permanentError represents an error that should not be retried by Cloud Tasks.
// Returning HTTP 2xx prevents Cloud Tasks from retrying the delivery.
type permanentError struct {
	msg string
}

func (e *permanentError) Error() string { return e.msg }
func (e *permanentError) Unwrap() error { return nil }

// IsPermanentError checks whether an error is non-retryable.
func IsPermanentError(err error) bool {
	var pe *permanentError
	return errors.As(err, &pe)
}

const maxHops = 15

const orchestratorSystemPrompt = `You are the orchestration agent for a multi-agent business intelligence system.
Your role is to examine the current state of a task and decide which specialized agent should act next.

Available agents:
1. "data" — Fetches external information: web search (Google), SEC EDGAR financial filings.
   Use when: the task requires factual data that has not yet been collected.
2. "analyst" — Generates and executes Python code for quantitative analysis, calculations, and charts (pandas, matplotlib).
   Use when: sufficient facts have been collected and analysis or visualization is needed.
3. "report" — Synthesizes collected facts and analysis into a structured final report.
   Use when: both data collection and analysis are complete.
4. "complete" — Finalize the task.
   Use when: final_result contains a report that adequately addresses the user's request.
5. "ask_user" — Ask the user a clarifying question.
   Use when: the user's request is ambiguous, lacks specific metrics, or you cannot determine how to proceed.
   Put your question in the "instructions" field.

You will receive the full job state as JSON. Examine:
- prompt: the user's INITIAL request (may be outdated if the user later clarified)
- collected_facts: structured data gathered so far (empty = no research yet)
- generated_assets: charts/analysis produced so far (empty = no analysis yet). Each asset has type, name, and for text assets a content_preview showing what was produced.
- missing_queries: unfulfilled data requests from previous agent runs
- final_result: the synthesized report text (empty = no report yet)
- last_agent_summary: feedback from the LAST agent that ran — what it did, what succeeded, what failed. Use this to decide the next step.
- chat_history: the FULL conversation including user clarifications

CRITICAL: If chat_history contains user replies AFTER an "ask_user" round, those replies
SUPERSEDE the original prompt. The user's LATEST message is the authoritative intent.
For example, if prompt says "TSLA IPO" but the user later clarified "I meant OpenAI",
you MUST act on the clarification, not the original prompt. Re-read the entire
chat_history to understand the evolved intent before making a routing decision.

Respond with JSON:
{
  "next_agent": "data|analyst|report|complete|ask_user",
  "reasoning": "one-sentence explanation of why this agent was chosen",
  "instructions": "specific actionable instructions for the chosen agent",
  "queries": ["search query 1", "query 2"]
}

Rules:
- Read last_agent_summary first to understand what just happened, then decide the next step.
- If no facts have been collected yet AND no analysis has been produced, route to "data" with specific search queries — UNLESS the task is purely computational (e.g., math, code execution) and needs no external data.
- If facts exist but no analysis/charts have been produced, route to "analyst".
- If the task needs no external data (pure computation, code execution), route directly to "analyst".
- If generated_assets is non-empty, the analyst has ALREADY SUCCEEDED. You MUST route to "report" — NEVER back to "analyst". Check the content_preview field to confirm analysis output exists, then proceed to report generation.
- If final_result is non-empty, the report has been written. Evaluate whether it adequately addresses the user's request. If yes, route to "complete". If the report is missing key information or is clearly inadequate, you may route to "data" or "analyst" for additional work — but only with specific instructions for what is missing.
- If the request is ambiguous or you need clarification, route to "ask_user" and put your question in "instructions".
- "queries" is required when next_agent is "data"; omit otherwise.
- Be specific in "instructions" — tell the agent exactly what data to find or what to compute.
- For financial requests, include company names/tickers in queries for SEC EDGAR lookup.
- When the user has clarified their intent in chat_history, use the clarified intent for queries and instructions.
- If collected_facts contains a "data_unavailable" entry, do NOT route to "data" again for the same topic. Instead, route to "report" to summarize what is known, or "ask_user" to explain the data limitation.
- Today's date is %s. Use this for any date-related reasoning.`

type RoutingDecision struct {
	NextAgent    string   `json:"next_agent"`
	Reasoning    string   `json:"reasoning"`
	Instructions string   `json:"instructions"`
	Queries      []string `json:"queries,omitempty"`
}

type OrchestratorAgent struct {
	gemini     LLMClient
	store      JobStore
	dispatcher TaskDispatcher
	selfURL    string
}

func NewOrchestratorAgent(
	gemini LLMClient, store JobStore, dispatcher TaskDispatcher,
	selfURL string,
) *OrchestratorAgent {
	return &OrchestratorAgent{
		gemini: gemini, store: store, dispatcher: dispatcher,
		selfURL: selfURL,
	}
}

// Execute runs one iteration of the orchestration loop:
// read blackboard → decide next agent → update Firestore → enqueue agent task.
// Agents are invoked asynchronously via Cloud Tasks, not synchronously.
func (o *OrchestratorAgent) Execute(ctx context.Context, jobID, sessionID string) error {
	job, err := o.store.GetJob(ctx, jobID, sessionID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return &permanentError{msg: fmt.Sprintf("job %s not found for session %s", jobID, sessionID)}
	}

	// Terminal states — no-op
	switch job.Status {
	case StatusCompleted, StatusFailed, StatusHITL:
		slog.Info("orchestrator: terminal state, skipping", "job_id", jobID, "session_id", sessionID, "status", job.Status)
		return nil
	}

	// Guard: only proceed if the orchestrator is the expected active agent.
	// Prevents duplicate /internal/route deliveries from overwriting an in-flight agent.
	if job.ActiveAgent != AgentOrchestrator {
		slog.Warn("orchestrator: not active agent, skipping duplicate delivery",
			"job_id", jobID, "session_id", sessionID, "active_agent", job.ActiveAgent)
		return nil
	}

	// Circuit breaker — atomic increment prevents race from duplicate deliveries
	newHop, err := o.store.IncrementHopCount(ctx, jobID, sessionID)
	if err != nil {
		return fmt.Errorf("increment hop count: %w", err)
	}
	if newHop > maxHops {
		slog.Warn("circuit breaker triggered", "job_id", jobID, "session_id", sessionID, "hop_count", newHop)
		return o.store.UpdateJob(ctx, jobID, sessionID, []firestore.Update{
			{Path: "status", Value: StatusHITL},
			{Path: "active_agent", Value: AgentOrchestrator},
			{Path: "agent_instructions", Value: "Maximum processing steps reached. Please refine your request or provide additional context."},
			{Path: "final_result", Value: ""},
		})
	}

	// Get session for chat context
	session, err := o.store.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	// LLM-driven routing decision
	decision, routeUsage, err := o.decide(ctx, job, session)
	if err != nil {
		detail := "routing decision failed: " + err.Error()
		if auditErr := o.store.AppendAuditLog(ctx, jobID, sessionID, AuditEntry{
			Agent:  AgentOrchestrator,
			Action: "route",
			Tokens: routeUsage,
			Detail: detail,
		}); auditErr != nil {
			slog.Error("audit log failed", "job_id", jobID, "session_id", sessionID, "agent", "orchestrator", "error", auditErr)
		}
		return o.failJob(ctx, job, detail)
	}

	// Programmatic guardrails — catch LLM routing errors that the prompt
	// rules can't guarantee. Overrides are logged so we can tune the prompt.
	decision = validateDecision(decision, job)

	if err := o.store.AppendAuditLog(ctx, jobID, sessionID, AuditEntry{
		Agent:  AgentOrchestrator,
		Action: "route",
		Tokens: routeUsage,
		Detail: fmt.Sprintf("next=%s: %s", decision.NextAgent, decision.Reasoning),
	}); err != nil {
		slog.Error("audit log failed", "job_id", jobID, "session_id", sessionID, "agent", "orchestrator", "error", err)
	}
	slog.Info("orchestrator: routing decision",
		"job_id", jobID, "session_id", sessionID, "next_agent", decision.NextAgent, "reasoning", decision.Reasoning,
	)

	// Dispatch to chosen agent via Cloud Tasks (async)
	switch decision.NextAgent {
	case "data":
		if err := o.store.UpdateJob(ctx, jobID, sessionID, []firestore.Update{
			{Path: "status", Value: StatusQueued},
			{Path: "active_agent", Value: AgentData},
			{Path: "agent_instructions", Value: decision.Instructions},
			{Path: "missing_queries", Value: decision.Queries},
		}); err != nil {
			return fmt.Errorf("update for data agent: %w", err)
		}
		if err := o.dispatcher.Enqueue(ctx, o.selfURL+"/internal/agent/data", jobID, sessionID); err != nil {
			o.revertDispatch(ctx, jobID, sessionID, AgentData, err)
			return fmt.Errorf("enqueue data agent: %w", err)
		}
		return nil

	case "analyst":
		if err := o.store.UpdateJob(ctx, jobID, sessionID, []firestore.Update{
			{Path: "status", Value: StatusQueued},
			{Path: "active_agent", Value: AgentAnalyst},
			{Path: "agent_instructions", Value: decision.Instructions},
		}); err != nil {
			return fmt.Errorf("update for analyst: %w", err)
		}
		if err := o.dispatcher.Enqueue(ctx, o.selfURL+"/internal/agent/analyst", jobID, sessionID); err != nil {
			o.revertDispatch(ctx, jobID, sessionID, AgentAnalyst, err)
			return fmt.Errorf("enqueue analyst: %w", err)
		}
		return nil

	case "report":
		if err := o.store.UpdateJob(ctx, jobID, sessionID, []firestore.Update{
			{Path: "status", Value: StatusQueued},
			{Path: "active_agent", Value: AgentReport},
			{Path: "agent_instructions", Value: decision.Instructions},
		}); err != nil {
			return fmt.Errorf("update for report: %w", err)
		}
		if err := o.dispatcher.Enqueue(ctx, o.selfURL+"/internal/agent/report", jobID, sessionID); err != nil {
			o.revertDispatch(ctx, jobID, sessionID, AgentReport, err)
			return fmt.Errorf("enqueue report: %w", err)
		}
		return nil

	case "ask_user":
		slog.Info("orchestrator: HITL — asking user for clarification", "job_id", jobID, "session_id", sessionID)
		// Append the agent's question to chat history so the LLM sees the full conversation.
		if err := o.store.AppendChatHistory(ctx, sessionID, ChatMessage{Role: "assistant", Content: decision.Instructions}); err != nil {
			slog.Error("orchestrator: failed to append HITL question to chat history", "job_id", jobID, "session_id", sessionID, "error", err)
		}
		return o.store.UpdateJob(ctx, jobID, sessionID, []firestore.Update{
			{Path: "status", Value: StatusHITL},
			{Path: "active_agent", Value: AgentOrchestrator},
			{Path: "agent_instructions", Value: decision.Instructions},
			{Path: "final_result", Value: ""},
		})

	case "complete":
		slog.Info("orchestrator: marking job complete", "job_id", jobID, "session_id", sessionID)
		// Mark COMPLETED before appending to chat_history. This ordering
		// ensures that if a duplicate /internal/route delivery arrives, the
		// terminal-state guard at the top of Execute returns early and the
		// report is not appended twice.
		if err := o.store.UpdateJob(ctx, jobID, sessionID, []firestore.Update{
			{Path: "status", Value: StatusCompleted},
			{Path: "active_agent", Value: AgentOrchestrator},
		}); err != nil {
			return fmt.Errorf("mark complete: %w", err)
		}
		// Append the final report to chat history so future sessions in this
		// thread have the full conversation context. Best-effort: the report
		// is already persisted in final_result.
		if job.FinalResult != "" {
			if err := o.store.AppendChatHistory(ctx, sessionID, ChatMessage{
				Role: "assistant", Content: job.FinalResult,
			}); err != nil {
				slog.Error("orchestrator: failed to append final report to chat history",
					"job_id", jobID, "session_id", sessionID, "error", err)
			}
		}
		return nil

	default:
		return o.failJob(ctx, job, "unknown agent: "+decision.NextAgent)
	}
}

// validateDecision applies programmatic guardrails to catch LLM routing errors
// that prompt rules can't guarantee. Returns a potentially modified decision.
//
// Rules:
//   - "data" MUST have queries; fallback to ask_user.
//   - "analyst" with no collected facts → redirect to "data" (except pure-computation prompts,
//     which the LLM handles; we can't detect that here, so we only guard the empty-facts case).
//   - "complete" with empty final_result → redirect to "report".
//   - "report" with no facts AND no assets → redirect to "ask_user".
func validateDecision(d *RoutingDecision, job *Job) *RoutingDecision {
	hasFacts := len(job.CollectedFacts) > 0
	hasAssets := len(job.GeneratedAssets) > 0
	hasReport := job.FinalResult != ""

	switch d.NextAgent {
	case "data":
		if len(d.Queries) == 0 {
			slog.Warn("routing guardrail: LLM chose 'data' without queries — overriding to ask_user",
				"job_id", job.JobID, "session_id", job.SessionID, "reasoning", d.Reasoning)
			return &RoutingDecision{
				NextAgent:    "ask_user",
				Reasoning:    fmt.Sprintf("guardrail override: LLM chose 'data' without queries (original: %s)", d.Reasoning),
				Instructions: "I need more details to search for the right information. Could you clarify what specific data you'd like me to find?",
			}
		}

	case "analyst":
		if !hasFacts && !hasAssets {
			slog.Warn("routing guardrail: LLM chose 'analyst' but no facts or assets exist — overriding to data",
				"job_id", job.JobID, "session_id", job.SessionID, "reasoning", d.Reasoning)
			return &RoutingDecision{
				NextAgent:    "data",
				Reasoning:    fmt.Sprintf("guardrail override: analyst needs data first (original: %s)", d.Reasoning),
				Instructions: d.Instructions,
				Queries:      []string{job.Prompt}, // Use the original prompt as a search query
			}
		}

	case "report":
		if !hasFacts && !hasAssets {
			slog.Warn("routing guardrail: LLM chose 'report' but no facts or assets exist — overriding to ask_user",
				"job_id", job.JobID, "session_id", job.SessionID, "reasoning", d.Reasoning)
			return &RoutingDecision{
				NextAgent:    "ask_user",
				Reasoning:    fmt.Sprintf("guardrail override: report has no content to synthesize (original: %s)", d.Reasoning),
				Instructions: "I don't have enough information yet to write a report. Could you provide more context or clarify your request?",
			}
		}

	case "complete":
		if !hasReport {
			slog.Warn("routing guardrail: LLM chose 'complete' but final_result is empty — overriding to report",
				"job_id", job.JobID, "session_id", job.SessionID, "reasoning", d.Reasoning)
			return &RoutingDecision{
				NextAgent:    "report",
				Reasoning:    fmt.Sprintf("guardrail override: cannot complete without a report (original: %s)", d.Reasoning),
				Instructions: "Synthesize all collected facts and analysis into a comprehensive report.",
			}
		}
	}

	return d // no override needed
}

func (o *OrchestratorAgent) decide(ctx context.Context, job *Job, session *Session) (*RoutingDecision, TokenUsage, error) {
	state := map[string]any{
		"prompt":             job.Prompt,
		"status":             job.Status,
		"collected_facts":    compactFacts(job.CollectedFacts),
		"generated_assets":   assetSummaries(job.GeneratedAssets),
		"missing_queries":    job.MissingQueries,
		"final_result":       truncateRunes(job.FinalResult, 2000),
		"last_agent_summary": job.LastAgentSummary,
	}
	if session != nil && len(session.ChatHistory) > 0 {
		state["chat_history"] = compactChatHistory(session.ChatHistory)
	}
	stateJSON, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return nil, TokenUsage{}, fmt.Errorf("marshal orchestrator state: %w", err)
	}
	prompt := fmt.Sprintf("Current job state:\n%s\n\nDecide which agent should act next.", string(stateJSON))

	var decision RoutingDecision
	systemPrompt := fmt.Sprintf(orchestratorSystemPrompt, time.Now().Format("2006-01-02"))
	usage, err := o.gemini.GenerateJSON(ctx, systemPrompt, prompt, &decision)
	if err != nil {
		return nil, usage, err
	}
	return &decision, usage, nil
}

func (o *OrchestratorAgent) failJob(ctx context.Context, job *Job, reason string) error {
	slog.Error("failing job", "job_id", job.JobID, "session_id", job.SessionID, "reason", reason)
	return o.store.UpdateJob(ctx, job.JobID, job.SessionID, []firestore.Update{
		{Path: "status", Value: StatusFailed},
		{Path: "final_result", Value: reason},
	})
}

// revertDispatch rolls the job back to QUEUED+orchestrator after a failed
// Enqueue. This allows the Cloud Tasks retry of /internal/route to re-execute
// the routing decision instead of being blocked by the active_agent guard.
func (o *OrchestratorAgent) revertDispatch(ctx context.Context, jobID, sessionID string, agent AgentType, enqueueErr error) {
	if revertErr := o.store.UpdateJob(ctx, jobID, sessionID, []firestore.Update{
		{Path: "status", Value: StatusQueued},
		{Path: "active_agent", Value: AgentOrchestrator},
	}); revertErr != nil {
		slog.Error("orchestrator: revert after enqueue failure also failed",
			"job_id", jobID, "session_id", sessionID, "agent", agent,
			"enqueue_error", enqueueErr, "revert_error", revertErr)
	}
}

const (
	maxFactsForLLM       = 40  // Cap facts sent to orchestrator LLM
	maxFactValueRunes    = 300 // Truncate individual fact values
	maxChatHistoryForLLM = 20  // Keep last N messages for routing context
)

// compactFacts deduplicates and truncates facts before sending to the LLM.
// The full facts array is preserved in Firestore; this only affects the
// routing decision context.
func compactFacts(facts []Fact) []Fact {
	if len(facts) == 0 {
		return facts
	}

	// Deduplicate by key (keep last occurrence — most recent data wins)
	seen := make(map[string]int, len(facts))
	deduped := make([]Fact, 0, len(facts))
	for _, f := range facts {
		if idx, ok := seen[f.Key]; ok {
			deduped[idx] = f // overwrite earlier occurrence
		} else {
			seen[f.Key] = len(deduped)
			deduped = append(deduped, f)
		}
	}

	// Cap total count
	if len(deduped) > maxFactsForLLM {
		deduped = deduped[len(deduped)-maxFactsForLLM:]
	}

	// Truncate individual values
	out := make([]Fact, len(deduped))
	for i, f := range deduped {
		out[i] = Fact{
			Key:    f.Key,
			Value:  truncateRunes(f.Value, maxFactValueRunes),
			Source: f.Source,
		}
	}
	return out
}

// compactChatHistory keeps only the most recent messages for routing context.
// Always preserves the first message (original user prompt) when truncating.
func compactChatHistory(history []ChatMessage) []ChatMessage {
	if len(history) <= maxChatHistoryForLLM {
		return history
	}
	// Keep first message + last (maxChatHistoryForLLM - 1) messages
	compact := make([]ChatMessage, 0, maxChatHistoryForLLM)
	compact = append(compact, history[0])
	tail := history[len(history)-(maxChatHistoryForLLM-1):]
	compact = append(compact, tail...)
	return compact
}

func assetSummaries(assets []Asset) []map[string]string {
	out := make([]map[string]string, len(assets))
	for i, a := range assets {
		m := map[string]string{"type": a.Type, "name": a.Name}
		// Include a content preview for text-based assets so the LLM can see
		// what was produced. Skip binary content (charts are base64-encoded).
		if a.Type != "chart" && a.Content != "" {
			m["content_preview"] = truncateRunes(a.Content, 500)
		}
		out[i] = m
	}
	return out
}
