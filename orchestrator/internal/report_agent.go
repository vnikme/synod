package internal

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"cloud.google.com/go/firestore"
)

const reportSystemPrompt = `You are a business intelligence report writer.
Synthesize the collected data and analysis into a clear, professional report.

Structure:
1. Executive Summary (2-3 sentences)
2. Key Findings (bullet points with specific numbers)
3. Analysis (reference any charts/visualizations produced)
4. Conclusion & Outlook

Rules:
- Use exact numbers from the collected facts.
- Reference charts by name (e.g., "See chart_1.png").
- Keep the language professional but accessible.
- Highlight trends, comparisons, and notable changes.
- Be comprehensive but concise.`

type ReportAgent struct {
	gemini *GeminiClient
	store  *Store
}

func NewReportAgent(gemini *GeminiClient, store *Store) *ReportAgent {
	return &ReportAgent{gemini: gemini, store: store}
}

func (a *ReportAgent) Execute(ctx context.Context, job *Job, session *Session, instructions string) (TokenUsage, error) {
	slog.Info("report agent: starting", "job_id", job.JobID)

	var prompt strings.Builder
	prompt.WriteString(fmt.Sprintf("Original request: %s\n\n", job.Prompt))
	prompt.WriteString(fmt.Sprintf("Report instructions: %s\n\n", instructions))

	prompt.WriteString("Collected facts:\n")
	for _, f := range job.CollectedFacts {
		prompt.WriteString(fmt.Sprintf("- %s: %s (source: %s)\n", f.Key, f.Value, f.Source))
	}
	prompt.WriteString("\n")

	prompt.WriteString("Analysis output:\n")
	for _, asset := range job.GeneratedAssets {
		switch asset.Type {
		case "analysis_output":
			prompt.WriteString(asset.Content + "\n")
		case "chart":
			prompt.WriteString(fmt.Sprintf("[Chart: %s]\n", asset.Name))
		}
	}
	prompt.WriteString("\n")

	if session != nil && len(session.ChatHistory) > 0 {
		prompt.WriteString("Conversation context:\n")
		for _, msg := range session.ChatHistory {
			prompt.WriteString(fmt.Sprintf("%s: %s\n", msg.Role, msg.Content))
		}
	}

	report, usage, err := a.gemini.GenerateText(ctx, reportSystemPrompt, prompt.String())
	if err != nil {
		return usage, fmt.Errorf("report generation: %w", err)
	}

	slog.Info("report agent: done", "job_id", job.JobID, "report_len", len(report))

	// Build summary for orchestrator
	summary := fmt.Sprintf("Report agent completed. Generated a %d-character report.", len(report))
	if report != "" {
		summary += "\nReport preview:\n" + truncateRunes(report, 300)
	}

	// Write final_result but do NOT set terminal state.
	// The orchestrator evaluates the report and decides whether to complete
	// the job or request revisions.
	if err := a.store.UpdateJob(ctx, job.JobID, job.SessionID, []firestore.Update{
		{Path: "final_result", Value: report},
		{Path: "last_agent_summary", Value: summary},
	}); err != nil {
		return usage, err
	}

	// Append report to session chat history for multi-turn context
	if session != nil {
		if err := a.store.AppendChatHistory(ctx, job.SessionID, ChatMessage{
			Role: "assistant", Content: report,
		}); err != nil {
			slog.Error("failed to append report to chat history", "error", err)
		}
	}

	return usage, nil
}
