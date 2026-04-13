package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/idtoken"
)

const maxCodeRetries = 3

const codeGenSystemPrompt = `You are a Python data analyst. Generate executable Python code for the requested analysis.
Today's date is %s.

Available libraries: pandas, numpy, matplotlib.pyplot, math, statistics, json, datetime, re.
You MUST import any library you use (e.g., import pandas as pd).

Rules:
- Use print() to output text results and insights.
- Use matplotlib to create charts. Call plt.figure() before each chart.
- Do NOT call plt.show() — charts are captured automatically.
- Do NOT read files or make network calls. All data must be embedded in the code.
- Embed the provided facts as Python data structures (lists, dicts, DataFrames).
- Keep code concise and focused on the requested analysis.
- Add brief comments explaining each analysis step.

Output ONLY the Python code, no markdown fences or explanation.`

type AnalystAgent struct {
	gemini     *GeminiClient
	store      *Store
	sandboxURL string
	http       *http.Client
}

func NewAnalystAgent(ctx context.Context, gemini *GeminiClient, store *Store, sandboxURL string) (*AnalystAgent, error) {
	// Create an HTTP client that automatically adds ID tokens for Cloud Run auth.
	// Falls back to a plain client if no credentials are available (local dev).
	httpClient, err := idtoken.NewClient(ctx, sandboxURL)
	if err != nil {
		slog.Warn("idtoken client unavailable, using plain HTTP (sandbox must allow unauthenticated)", "error", err)
		httpClient = &http.Client{Timeout: 240 * time.Second}
	} else {
		httpClient.Timeout = 240 * time.Second
	}
	return &AnalystAgent{
		gemini:     gemini,
		store:      store,
		sandboxURL: sandboxURL,
		http:       httpClient,
	}, nil
}

func (a *AnalystAgent) Execute(ctx context.Context, job *Job, instructions string) (TokenUsage, error) {
	slog.Info("analyst agent: starting", "job_id", job.JobID)

	// Build facts context
	var factsText strings.Builder
	for _, f := range job.CollectedFacts {
		factsText.WriteString(fmt.Sprintf("- %s: %s (source: %s)\n", f.Key, f.Value, f.Source))
	}

	prompt := fmt.Sprintf(
		"User request: %s\n\nAnalysis instructions: %s\n\nAvailable data:\n%s\n\nGenerate Python code.",
		job.Prompt, instructions, factsText.String(),
	)

	var lastError string
	var totalUsage TokenUsage
	for attempt := 1; attempt <= maxCodeRetries; attempt++ {
		p := prompt
		if lastError != "" {
			p += fmt.Sprintf("\n\nYour previous code failed with error:\n%s\n\nFix the issues and regenerate.", lastError)
		}

		code, usage, err := a.gemini.GenerateText(ctx, fmt.Sprintf(codeGenSystemPrompt, time.Now().Format("2006-01-02")), p)
		if err != nil {
			return totalUsage, fmt.Errorf("code generation: %w", err)
		}
		totalUsage = totalUsage.Add(usage)
		code = stripCodeFences(code)

		slog.Info("analyst agent: executing code", "job_id", job.JobID, "attempt", attempt, "code_len", len(code))

		result, err := a.callSandbox(ctx, code)
		if err != nil {
			slog.Warn("analyst agent: sandbox call failed (transient)",
				"job_id", job.JobID, "attempt", attempt, "error", err)
			lastError = fmt.Sprintf("sandbox HTTP error: %s", err.Error())
			continue
		}

		if result.Success {
			slog.Info("analyst agent: success", "job_id", job.JobID, "charts", len(result.Charts))
			assets := append([]Asset{}, job.GeneratedAssets...)
			for i, chart := range result.Charts {
				assets = append(assets, Asset{
					Type:    "chart",
					Name:    fmt.Sprintf("chart_%d.png", i+1),
					Content: chart,
				})
			}
			if result.Stdout != "" {
				assets = append(assets, Asset{
					Type:    "analysis_output",
					Name:    "analysis.txt",
					Content: result.Stdout,
				})
			}

			// Build summary for orchestrator
			var summaryParts []string
			summaryParts = append(summaryParts, fmt.Sprintf("Analyst agent completed successfully. Charts produced: %d.", len(result.Charts)))
			if result.Stdout != "" {
				preview := result.Stdout
				if len(preview) > 500 {
					preview = preview[:500] + "…"
				}
				summaryParts = append(summaryParts, "Analysis output:\n"+preview)
			}
			summary := strings.Join(summaryParts, "\n")

			return totalUsage, a.store.UpdateJob(ctx, job.JobID, job.SessionID, []firestore.Update{
				{Path: "generated_assets", Value: assets},
				{Path: "last_agent_summary", Value: summary},
			})
		}

		slog.Warn("analyst agent: code failed", "job_id", job.JobID, "attempt", attempt, "error", result.Error)
		lastError = result.Error
	}

	return totalUsage, fmt.Errorf("code execution failed after %d attempts: %s", maxCodeRetries, lastError)
}

func (a *AnalystAgent) callSandbox(ctx context.Context, code string) (*SandboxResponse, error) {
	body, err := json.Marshal(SandboxRequest{Code: code})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", a.sandboxURL+"/execute", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sandbox HTTP: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("sandbox returned %d: %.500s", resp.StatusCode, respBody)
	}

	var result SandboxResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode sandbox response: %w", err)
	}
	return &result, nil
}

func stripCodeFences(code string) string {
	code = strings.TrimSpace(code)
	if strings.HasPrefix(code, "```python") {
		code = strings.TrimPrefix(code, "```python")
	} else if strings.HasPrefix(code, "```") {
		code = strings.TrimPrefix(code, "```")
	}
	if strings.HasSuffix(code, "```") {
		code = strings.TrimSuffix(code, "```")
	}
	return strings.TrimSpace(code)
}
