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

const maxHops = 5

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

You will receive the full job state as JSON. Examine:
- prompt: the user's original request
- collected_facts: structured data gathered so far (empty = no research yet)
- generated_assets: charts/analysis produced so far (empty = no analysis yet)
- missing_queries: unfulfilled data requests from previous agent runs
- final_result: final output text (empty = not done yet)

Respond with JSON:
{
  "next_agent": "data|analyst|report|complete",
  "reasoning": "one-sentence explanation of why this agent was chosen",
  "instructions": "specific actionable instructions for the chosen agent",
  "queries": ["search query 1", "query 2"]
}

Rules:
- If no facts have been collected yet, route to "data" with specific search queries.
- If facts exist but no analysis/charts have been produced, route to "analyst".
- If both facts and analysis artifacts exist, route to "report".
- If final_result is already populated, return "complete".
- "queries" is required when next_agent is "data"; omit otherwise.
- Be specific in "instructions" — tell the agent exactly what data to find or what to compute.
- For financial requests, include company names/tickers in queries for SEC EDGAR lookup.`

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
	data       *DataAgent
	analyst    *AnalystAgent
	report     *ReportAgent
	selfURL    string
}

func NewOrchestratorAgent(
	gemini *GeminiClient, store *Store, dispatcher *Dispatcher,
	data *DataAgent, analyst *AnalystAgent, report *ReportAgent,
	selfURL string,
) *OrchestratorAgent {
	return &OrchestratorAgent{
		gemini: gemini, store: store, dispatcher: dispatcher,
		data: data, analyst: analyst, report: report,
		selfURL: selfURL,
	}
}

// Execute runs one iteration of the orchestration loop:
// read blackboard → decide next agent → execute agent → enqueue next hop if needed.
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

	// Circuit breaker — atomic increment prevents race from duplicate deliveries
	newHop, err := o.store.IncrementHopCount(ctx, jobID, sessionID)
	if err != nil {
		return fmt.Errorf("increment hop count: %w", err)
	}
	if newHop > maxHops {
		slog.Warn("circuit breaker triggered", "job_id", jobID, "hop_count", newHop)
		return o.store.UpdateJob(ctx, jobID, sessionID, []firestore.Update{
			{Path: "status", Value: StatusHITL},
		})
	}

	// Get session for chat context
	session, err := o.store.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	// LLM-driven routing decision
	decision, err := o.decide(ctx, job, session)
	if err != nil {
		return o.failJob(ctx, job, "routing decision failed: "+err.Error())
	}
	slog.Info("orchestrator: routing decision",
		"job_id", jobID, "next_agent", decision.NextAgent, "reasoning", decision.Reasoning,
	)

	// Dispatch to chosen agent
	switch decision.NextAgent {
	case "data":
		if len(decision.Queries) == 0 {
			return o.failJob(ctx, job, "routing decided 'data' but provided no queries")
		}
		if err := o.store.UpdateJob(ctx, jobID, sessionID, []firestore.Update{
			{Path: "status", Value: StatusInProgress},
			{Path: "active_agent", Value: AgentData},
			{Path: "agent_instructions", Value: decision.Instructions},
			{Path: "missing_queries", Value: decision.Queries},
		}); err != nil {
			return fmt.Errorf("update for data agent: %w", err)
		}
		if err := o.data.Execute(ctx, job, decision.Instructions, decision.Queries); err != nil {
			slog.Error("data agent failed", "error", err)
			return o.failJob(ctx, job, "data agent: "+err.Error())
		}

	case "analyst":
		if err := o.store.UpdateJob(ctx, jobID, sessionID, []firestore.Update{
			{Path: "status", Value: StatusInProgress},
			{Path: "active_agent", Value: AgentAnalyst},
			{Path: "agent_instructions", Value: decision.Instructions},
		}); err != nil {
			return fmt.Errorf("update for analyst: %w", err)
		}
		if err := o.analyst.Execute(ctx, job, decision.Instructions); err != nil {
			slog.Error("analyst agent failed", "error", err)
			return o.failJob(ctx, job, "analyst agent: "+err.Error())
		}

	case "report":
		if err := o.store.UpdateJob(ctx, jobID, sessionID, []firestore.Update{
			{Path: "status", Value: StatusInProgress},
			{Path: "active_agent", Value: AgentReport},
			{Path: "agent_instructions", Value: decision.Instructions},
		}); err != nil {
			return fmt.Errorf("update for report: %w", err)
		}
		// Re-read job to get latest facts/assets written by previous agents
		job, err = o.store.GetJob(ctx, jobID, sessionID)
		if err != nil {
			return fmt.Errorf("re-read job for report: %w", err)
		}
		if err := o.report.Execute(ctx, job, session, decision.Instructions); err != nil {
			slog.Error("report agent failed", "error", err)
			return o.failJob(ctx, job, "report agent: "+err.Error())
		}

	case "complete":
		slog.Info("orchestrator: task already complete", "job_id", jobID)
		return nil

	default:
		return o.failJob(ctx, job, "unknown agent: "+decision.NextAgent)
	}

	// Check if the agent reached a terminal state
	job, err = o.store.GetJob(ctx, jobID, sessionID)
	if err != nil {
		return fmt.Errorf("re-read job after agent: %w", err)
	}
	if job.Status == StatusCompleted || job.Status == StatusFailed || job.Status == StatusHITL {
		slog.Info("orchestrator: agent reached terminal state", "job_id", jobID, "status", job.Status)
		return nil
	}

	// Enqueue next orchestration iteration
	routeURL := o.selfURL + "/internal/route"
	if err := o.dispatcher.Enqueue(ctx, routeURL, jobID, sessionID); err != nil {
		return fmt.Errorf("enqueue next hop: %w", err)
	}
	return nil
}

func (o *OrchestratorAgent) decide(ctx context.Context, job *Job, session *Session) (*RoutingDecision, error) {
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
		return nil, fmt.Errorf("marshal orchestrator state: %w", err)
	}
	prompt := fmt.Sprintf("Current job state:\n%s\n\nDecide which agent should act next.", string(stateJSON))

	var decision RoutingDecision
	if err := o.gemini.GenerateJSON(ctx, orchestratorSystemPrompt, prompt, &decision); err != nil {
		return nil, err
	}
	return &decision, nil
}

func (o *OrchestratorAgent) failJob(ctx context.Context, job *Job, reason string) error {
	slog.Error("failing job", "job_id", job.JobID, "reason", reason)
	return o.store.UpdateJob(ctx, job.JobID, job.SessionID, []firestore.Update{
		{Path: "status", Value: StatusFailed},
		{Path: "final_result", Value: reason},
	})
}

func assetSummaries(assets []Asset) []map[string]string {
	out := make([]map[string]string, len(assets))
	for i, a := range assets {
		out[i] = map[string]string{"type": a.Type, "name": a.Name}
	}
	return out
}
