package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"google.golang.org/genai"
)

const maxPlanRetries = 3

const plannerSystemPrompt = `You are a task planner for a multi-agent business intelligence system.
Given a user's business request and optional conversation history, decompose the request into sub-tasks.

You MUST respond with valid JSON matching this exact schema:
{
  "research_queries": ["list of specific search queries or data requests the researcher agent should fulfill"],
  "analysis_instructions": "detailed instructions for the analyst agent on what analysis/charts to produce",
  "needs_research": true/false,
  "needs_analysis": true/false
}

Rules:
- If the request involves looking up data, financial information, or facts: set needs_research=true and provide research_queries.
- If the request involves creating charts, calculations, summaries, or data analysis: set needs_analysis=true and provide analysis_instructions.
- research_queries should be specific and actionable.
- analysis_instructions should describe exactly what code to generate.
- Most business intelligence requests need both research AND analysis.`

type Planner struct {
	client *genai.Client
	model  string
}

func NewPlanner(ctx context.Context) (*Planner, error) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY is required")
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("genai.NewClient: %w", err)
	}
	model := os.Getenv("LLM_MODEL")
	if model == "" {
		model = "gemini-2.0-flash"
	}
	return &Planner{client: client, model: model}, nil
}

func (p *Planner) Plan(ctx context.Context, prompt string, chatHistory []ChatMessage) (*TaskPlan, error) {
	var userContent strings.Builder
	if len(chatHistory) > 0 {
		userContent.WriteString("Previous conversation:\n")
		for _, msg := range chatHistory {
			userContent.WriteString(fmt.Sprintf("%s: %s\n", msg.Role, msg.Content))
		}
		userContent.WriteString("\n")
	}
	userContent.WriteString("Current request: " + prompt)

	var lastErr error
	for attempt := 1; attempt <= maxPlanRetries; attempt++ {
		promptText := userContent.String()
		if lastErr != nil {
			promptText += fmt.Sprintf("\n\nYour previous response was invalid JSON. Error: %s\nPlease respond with valid JSON only.", lastErr.Error())
		}

		resp, err := p.client.Models.GenerateContent(ctx, p.model,
			genai.Text(promptText),
			&genai.GenerateContentConfig{
				SystemInstruction: genai.NewContentFromText(plannerSystemPrompt, "user"),
				ResponseMIMEType:  "application/json",
			},
		)
		if err != nil {
			return nil, fmt.Errorf("gemini GenerateContent (attempt %d): %w", attempt, err)
		}

		text := resp.Text()
		var plan TaskPlan
		if err := json.Unmarshal([]byte(text), &plan); err != nil {
			lastErr = fmt.Errorf("attempt %d: %w (response: %s)", attempt, err, truncate(text, 200))
			slog.Warn("plan parse failed, retrying", "attempt", attempt, "error", err)
			continue
		}

		slog.Info("plan generated", "needs_research", plan.NeedsResearch, "needs_analysis", plan.NeedsAnalysis, "queries", len(plan.ResearchQueries))
		return &plan, nil
	}
	return nil, fmt.Errorf("planner failed after %d attempts: %w", maxPlanRetries, lastErr)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
