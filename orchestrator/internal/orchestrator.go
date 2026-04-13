package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

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
4. "complete" — The task is already finished.
   Use when: final_result is already set.
5. "ask_user" — Ask the user a clarifying question.
   Use when: the user's request is ambiguous, lacks specific metrics, or you cannot determine how to proceed.
   Put your question in the "instructions" field.

You will receive the full job state as JSON. Examine:
- prompt: the user's INITIAL request (may be outdated if the user later clarified)
- collected_facts: structured data gathered so far (empty = no research yet)
- generated_assets: charts/analysis produced so far (empty = no analysis yet)
- missing_queries: unfulfilled data requests from previous agent runs
- final_result: final output text (empty = not done yet)
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
- If no facts have been collected yet, route to "data" with specific search queries.
- If facts exist but no analysis/charts have been produced, route to "analyst".
- If both facts and analysis artifacts exist, route to "report".
- If final_result is already populated, return "complete".
- If the request is ambiguous or you need clarification, route to "ask_user" and put your question in "instructions".
- "queries" is required when next_agent is "data"; omit otherwise.
- Be specific in "instructions" — tell the agent exactly what data to find or what to compute.
- For financial requests, include company names/tickers in queries for SEC EDGAR lookup.
- When the user has clarified their intent in chat_history, use the clarified intent for queries and instructions.`

type RoutingDecision struct {
	NextAgent    string   `json:"next_agent"`
	Reasoning    string   `json:"reasoning"`
	Instructions string   `json:"instructions"`
	Queries      []string `json:"queries,omitempty"`
}

type OrchestratorAgent struct {
	gemini     *GeminiClient
	store      *Store
	dispatcher *Dispatcher
	selfURL    string
}

func NewOrchestratorAgent(
	gemini *GeminiClient, store *Store, dispatcher *Dispatcher,
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
		slog.Info("orchestrator: terminal state, skipping", "job_id", jobID, "status", job.Status)
		return nil
	}

	// Guard: only proceed if the orchestrator is the expected active agent.
	// Prevents duplicate /internal/route deliveries from overwriting an in-flight agent.
	if job.ActiveAgent != AgentOrchestrator {
		slog.Warn("orchestrator: not active agent, skipping duplicate delivery",
			"job_id", jobID, "active_agent", job.ActiveAgent)
		return nil
	}

	// Circuit breaker — atomic increment prevents race from duplicate deliveries
	newHop, err := o.store.IncrementHopCount(ctx, jobID, sessionID)
	if err != nil {
		return fmt.Errorf("increment hop count: %w", err)
	}
	if newHop > maxHops {
		slog.Warn("circuit breaker triggered", "job_id", jobID, "hop_count", newHop)
		return o.store.UpdateJob(ctx, jobID, sessionID, []firestore.Update{
			{Path: "status", Value: StatusHITL},
			{Path: "active_agent", Value: AgentOrchestrator},
			{Path: "final_result", Value: "Maximum processing steps reached. Please refine your request or provide additional context."},
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
			slog.Error("audit log failed", "job_id", jobID, "agent", "orchestrator", "error", auditErr)
		}
		return o.failJob(ctx, job, detail)
	}
	if err := o.store.AppendAuditLog(ctx, jobID, sessionID, AuditEntry{
		Agent:  AgentOrchestrator,
		Action: "route",
		Tokens: routeUsage,
		Detail: fmt.Sprintf("next=%s: %s", decision.NextAgent, decision.Reasoning),
	}); err != nil {
		slog.Error("audit log failed", "job_id", jobID, "agent", "orchestrator", "error", err)
	}
	slog.Info("orchestrator: routing decision",
		"job_id", jobID, "next_agent", decision.NextAgent, "reasoning", decision.Reasoning,
	)

	// Dispatch to chosen agent via Cloud Tasks (async)
	switch decision.NextAgent {
	case "data":
		if len(decision.Queries) == 0 {
			return o.failJob(ctx, job, "routing decided 'data' but provided no queries")
		}
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
		slog.Info("orchestrator: HITL — asking user for clarification", "job_id", jobID)
		return o.store.UpdateJob(ctx, jobID, sessionID, []firestore.Update{
			{Path: "status", Value: StatusHITL},
			{Path: "active_agent", Value: AgentOrchestrator},
			{Path: "final_result", Value: decision.Instructions},
		})

	case "complete":
		slog.Info("orchestrator: task already complete", "job_id", jobID)
		return nil

	default:
		return o.failJob(ctx, job, "unknown agent: "+decision.NextAgent)
	}
}

func (o *OrchestratorAgent) decide(ctx context.Context, job *Job, session *Session) (*RoutingDecision, TokenUsage, error) {
	state := map[string]any{
		"prompt":           job.Prompt,
		"status":           job.Status,
		"collected_facts":  job.CollectedFacts,
		"generated_assets": assetSummaries(job.GeneratedAssets),
		"missing_queries":  job.MissingQueries,
		"final_result":     job.FinalResult,
	}
	if session != nil && len(session.ChatHistory) > 0 {
		state["chat_history"] = session.ChatHistory
	}
	stateJSON, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return nil, TokenUsage{}, fmt.Errorf("marshal orchestrator state: %w", err)
	}
	prompt := fmt.Sprintf("Current job state:\n%s\n\nDecide which agent should act next.", string(stateJSON))

	var decision RoutingDecision
	usage, err := o.gemini.GenerateJSON(ctx, orchestratorSystemPrompt, prompt, &decision)
	if err != nil {
		return nil, usage, err
	}
	return &decision, usage, nil
}

func (o *OrchestratorAgent) failJob(ctx context.Context, job *Job, reason string) error {
	slog.Error("failing job", "job_id", job.JobID, "reason", reason)
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
			"job_id", jobID, "agent", agent,
			"enqueue_error", enqueueErr, "revert_error", revertErr)
	}
}

func assetSummaries(assets []Asset) []map[string]string {
	out := make([]map[string]string, len(assets))
	for i, a := range assets {
		out[i] = map[string]string{"type": a.Type, "name": a.Name}
	}
	return out
}
