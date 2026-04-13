package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"google.golang.org/genai"
)

type GeminiClient struct {
	client *genai.Client
	model  string
}

func NewGeminiClient(ctx context.Context, apiKey, model string) (*GeminiClient, error) {
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
	if model == "" {
		model = "gemini-2.0-flash"
	}
	return &GeminiClient{client: client, model: model}, nil
}

// extractUsage extracts token counts from a Gemini response.
func extractUsage(resp *genai.GenerateContentResponse) TokenUsage {
	if resp == nil || resp.UsageMetadata == nil {
		return TokenUsage{}
	}
	var prompt, completion int
	if resp.UsageMetadata.PromptTokenCount != nil {
		prompt = int(*resp.UsageMetadata.PromptTokenCount)
	}
	if resp.UsageMetadata.CandidatesTokenCount != nil {
		completion = int(*resp.UsageMetadata.CandidatesTokenCount)
	}
	return TokenUsage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
	}
}

// GenerateJSON calls Gemini with JSON mode and unmarshals into out.
// Self-corrects on parse failures up to 3 attempts.
// Returns accumulated token usage across all attempts.
func (g *GeminiClient) GenerateJSON(ctx context.Context, system, prompt string, out any) (TokenUsage, error) {
	const maxRetries = 3
	var lastErr error
	var totalUsage TokenUsage
	for attempt := 1; attempt <= maxRetries; attempt++ {
		p := prompt
		if lastErr != nil {
			p += fmt.Sprintf("\n\nPrevious response was invalid JSON. Error: %s\nRespond with valid JSON only.", lastErr)
		}
		resp, err := g.client.Models.GenerateContent(ctx, g.model,
			genai.Text(p),
			&genai.GenerateContentConfig{
				SystemInstruction: genai.NewContentFromText(system, "user"),
				ResponseMIMEType:  "application/json",
			},
		)
		if err != nil {
			return totalUsage, fmt.Errorf("GenerateContent (attempt %d): %w", attempt, err)
		}
		totalUsage = totalUsage.Add(extractUsage(resp))
		text := resp.Text()
		if err := json.Unmarshal([]byte(text), out); err != nil {
			lastErr = fmt.Errorf("attempt %d: %w (response: %.200s)", attempt, err, text)
			slog.Warn("JSON parse failed, retrying", "attempt", attempt, "error", err)
			continue
		}
		return totalUsage, nil
	}
	return totalUsage, fmt.Errorf("GenerateJSON failed after %d attempts: %w", maxRetries, lastErr)
}

// GenerateText calls Gemini and returns the raw text response and token usage.
func (g *GeminiClient) GenerateText(ctx context.Context, system, prompt string) (string, TokenUsage, error) {
	resp, err := g.client.Models.GenerateContent(ctx, g.model,
		genai.Text(prompt),
		&genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText(system, "user"),
		},
	)
	if err != nil {
		return "", TokenUsage{}, fmt.Errorf("GenerateContent: %w", err)
	}
	return resp.Text(), extractUsage(resp), nil
}

// SearchResult represents a single web search result from Gemini grounding.
type SearchResult struct {
	Title   string
	Snippet string
	URL     string
}

// SearchWeb performs a web search using Gemini's built-in Google Search grounding.
// Returns structured search results and the LLM-summarized answer.
func (g *GeminiClient) SearchWeb(ctx context.Context, query string) ([]SearchResult, string, TokenUsage, error) {
	resp, err := g.client.Models.GenerateContent(ctx, g.model,
		genai.Text(query),
		&genai.GenerateContentConfig{
			Tools: []*genai.Tool{
				{GoogleSearch: &genai.GoogleSearch{}},
			},
		},
	)
	if err != nil {
		return nil, "", TokenUsage{}, fmt.Errorf("SearchWeb: %w", err)
	}
	usage := extractUsage(resp)
	text := resp.Text()

	var results []SearchResult
	if len(resp.Candidates) > 0 && resp.Candidates[0].GroundingMetadata != nil {
		for _, chunk := range resp.Candidates[0].GroundingMetadata.GroundingChunks {
			if chunk.Web != nil {
				results = append(results, SearchResult{
					Title: chunk.Web.Title,
					URL:   chunk.Web.URI,
				})
			}
		}
	}
	return results, text, usage, nil
}
