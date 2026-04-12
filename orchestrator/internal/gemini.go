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

// GenerateJSON calls Gemini with JSON mode and unmarshals into out.
// Self-corrects on parse failures up to 3 attempts.
func (g *GeminiClient) GenerateJSON(ctx context.Context, system, prompt string, out any) error {
	const maxRetries = 3
	var lastErr error
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
			return fmt.Errorf("GenerateContent (attempt %d): %w", attempt, err)
		}
		text := resp.Text()
		if err := json.Unmarshal([]byte(text), out); err != nil {
			lastErr = fmt.Errorf("attempt %d: %w (response: %.200s)", attempt, err, text)
			slog.Warn("JSON parse failed, retrying", "attempt", attempt, "error", err)
			continue
		}
		return nil
	}
	return fmt.Errorf("GenerateJSON failed after %d attempts: %w", maxRetries, lastErr)
}

// GenerateText calls Gemini and returns the raw text response.
func (g *GeminiClient) GenerateText(ctx context.Context, system, prompt string) (string, error) {
	resp, err := g.client.Models.GenerateContent(ctx, g.model,
		genai.Text(prompt),
		&genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText(system, "user"),
		},
	)
	if err != nil {
		return "", fmt.Errorf("GenerateContent: %w", err)
	}
	return resp.Text(), nil
}
