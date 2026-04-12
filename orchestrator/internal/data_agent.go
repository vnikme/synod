package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
)

// Ticker → 10-digit CIK mapping for SEC EDGAR.
var tickerCIK = map[string]string{
	"AAPL": "0000320193", "MSFT": "0000789019", "GOOGL": "0001652044",
	"GOOG": "0001652044", "AMZN": "0001018724", "META": "0001326801",
	"TSLA": "0001318605", "NVDA": "0001045810", "JPM": "0000019617",
	"V": "0001403161", "WMT": "0000104169", "JNJ": "0000200406",
	"UNH": "0000731766", "XOM": "0000034088", "BAC": "0000070858",
	"PG": "0000080424", "DIS": "0001744489", "NFLX": "0001065280",
}

// Company name → CIK for natural language detection.
var nameCIK = map[string]string{
	"APPLE": "0000320193", "MICROSOFT": "0000789019",
	"GOOGLE": "0001652044", "ALPHABET": "0001652044",
	"AMAZON": "0001018724", "TESLA": "0001318605",
	"NVIDIA": "0001045810", "JPMORGAN": "0000019617",
	"WALMART": "0000104169", "NETFLIX": "0001065280",
	"DISNEY": "0001744489", "PROCTER": "0000080424",
}

const factExtractionPrompt = `Extract structured facts from the provided raw data.
Return a JSON array of objects: [{"key": "metric name", "value": "value with units", "source": "data source URL or name"}]
Only extract facts relevant to the given instructions.
Be precise with numbers. Include units and time periods.
Limit to the most important 15-20 facts.`

type DataAgent struct {
	gemini  *GeminiClient
	store   *Store
	cseKey  string
	cseCX   string
	edgarUA string
	http    *http.Client
}

func NewDataAgent(gemini *GeminiClient, store *Store, cseKey, cseCX, edgarUA string) *DataAgent {
	return &DataAgent{
		gemini:  gemini,
		store:   store,
		cseKey:  cseKey,
		cseCX:   cseCX,
		edgarUA: edgarUA,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (a *DataAgent) Execute(ctx context.Context, job *Job, instructions string, queries []string) (TokenUsage, error) {
	slog.Info("data agent: starting", "job_id", job.JobID, "num_queries", len(queries))

	var rawChunks []string

	for _, query := range queries {
		// Try SEC EDGAR if the query mentions a known company
		if cik := detectCIK(query); cik != "" {
			data, err := a.fetchEDGAR(ctx, cik, query)
			if err != nil {
				slog.Warn("EDGAR fetch failed", "query", query, "error", err)
			} else {
				rawChunks = append(rawChunks, fmt.Sprintf("[SEC EDGAR] %s:\n%s", query, data))
			}
		}

		// Web search for broader context
		results, err := a.searchWeb(ctx, query)
		if err != nil {
			slog.Warn("web search failed", "query", query, "error", err)
			continue
		}
		if results != "" {
			rawChunks = append(rawChunks, fmt.Sprintf("[Web Search] '%s':\n%s", query, results))
		}
	}

	if len(rawChunks) == 0 {
		slog.Warn("data agent: no data collected", "job_id", job.JobID)
		return TokenUsage{}, a.store.UpdateJob(ctx, job.JobID, job.SessionID, []firestore.Update{
			{Path: "status", Value: StatusNeedsCtx},
			{Path: "missing_queries", Value: queries},
		})
	}

	// LLM-extract structured facts from raw data
	combined := strings.Join(rawChunks, "\n\n---\n\n")
	if len(combined) > 30000 {
		combined = combined[:30000] + "\n...[truncated]"
	}
	prompt := fmt.Sprintf("Instructions: %s\n\nRaw data:\n%s", instructions, combined)

	var facts []Fact
	usage, err := a.gemini.GenerateJSON(ctx, factExtractionPrompt, prompt, &facts)
	if err != nil {
		return TokenUsage{}, fmt.Errorf("fact extraction: %w", err)
	}

	allFacts := append(job.CollectedFacts, facts...)
	slog.Info("data agent: done", "job_id", job.JobID, "new_facts", len(facts), "total_facts", len(allFacts))

	return usage, a.store.UpdateJob(ctx, job.JobID, job.SessionID, []firestore.Update{
		{Path: "collected_facts", Value: allFacts},
		{Path: "missing_queries", Value: []string{}},
	})
}

// --- Web Search ---

func (a *DataAgent) searchWeb(ctx context.Context, query string) (string, error) {
	if a.cseKey == "" || a.cseCX == "" {
		return "", fmt.Errorf("Google CSE not configured")
	}
	u := fmt.Sprintf("https://www.googleapis.com/customsearch/v1?key=%s&cx=%s&q=%s&num=5",
		url.QueryEscape(a.cseKey), url.QueryEscape(a.cseCX), url.QueryEscape(query))

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("CSE returned %d: %.200s", resp.StatusCode, body)
	}

	var cseResp struct {
		Items []struct {
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
			Link    string `json:"link"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cseResp); err != nil {
		return "", fmt.Errorf("decode CSE response: %w", err)
	}

	var sb strings.Builder
	for i, item := range cseResp.Items {
		sb.WriteString(fmt.Sprintf("%d. %s\n   %s\n   Source: %s\n\n", i+1, item.Title, item.Snippet, item.Link))
	}
	return sb.String(), nil
}

// --- SEC EDGAR ---

func (a *DataAgent) fetchEDGAR(ctx context.Context, cik, query string) (string, error) {
	edgarURL := fmt.Sprintf("https://data.sec.gov/api/xbrl/companyfacts/CIK%s.json", cik)
	req, err := http.NewRequestWithContext(ctx, "GET", edgarURL, nil)
	if err != nil {
		return "", err
	}
	ua := a.edgarUA
	if ua == "" {
		ua = "Synod/1.0 (synod@example.com)"
	}
	req.Header.Set("User-Agent", ua)

	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("EDGAR returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var data struct {
		EntityName string `json:"entityName"`
		Facts      struct {
			USGAAP map[string]struct {
				Label string `json:"label"`
				Units map[string][]struct {
					End   string  `json:"end"`
					Val   float64 `json:"val"`
					Form  string  `json:"form"`
					FY    int     `json:"fy"`
					FP    string  `json:"fp"`
					Filed string  `json:"filed"`
				} `json:"units"`
			} `json:"us-gaap"`
		} `json:"facts"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", fmt.Errorf("parse EDGAR: %w", err)
	}

	keyMetrics := []string{
		"Revenues", "RevenueFromContractWithCustomerExcludingAssessedTax",
		"NetIncomeLoss", "EarningsPerShareDiluted",
		"GrossProfit", "OperatingIncomeLoss", "Assets", "StockholdersEquity",
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Company: %s\n\n", data.EntityName))
	for _, metric := range keyMetrics {
		entry, ok := data.Facts.USGAAP[metric]
		if !ok {
			continue
		}
		for unit, entries := range entry.Units {
			start := 0
			if len(entries) > 8 {
				start = len(entries) - 8
			}
			sb.WriteString(fmt.Sprintf("%s (%s):\n", entry.Label, unit))
			for _, e := range entries[start:] {
				sb.WriteString(fmt.Sprintf("  %s (FY%d %s, %s): %g\n", e.End, e.FY, e.FP, e.Form, e.Val))
			}
			sb.WriteString("\n")
		}
	}
	return sb.String(), nil
}

// --- Ticker Detection ---

// tickerPatterns is precompiled at init to avoid per-call regex compilation.
var tickerPatterns = func() map[string]*regexp.Regexp {
	patterns := make(map[string]*regexp.Regexp, len(tickerCIK))
	for ticker := range tickerCIK {
		patterns[ticker] = regexp.MustCompile(`\b` + regexp.QuoteMeta(ticker) + `\b`)
	}
	return patterns
}()

func detectCIK(query string) string {
	upper := strings.ToUpper(query)

	// Find earliest positional match among tickers for deterministic results
	// when queries mention multiple companies.
	type match struct {
		cik string
		pos int
	}
	var bestTicker *match
	for ticker, cik := range tickerCIK {
		loc := tickerPatterns[ticker].FindStringIndex(upper)
		if loc != nil {
			if bestTicker == nil || loc[0] < bestTicker.pos {
				bestTicker = &match{cik: cik, pos: loc[0]}
			}
		}
	}
	if bestTicker != nil {
		return bestTicker.cik
	}

	// Tokenize query for word-boundary matching against company names.
	tokens := strings.FieldsFunc(upper, func(r rune) bool {
		return (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	})
	tokenStr := " " + strings.Join(tokens, " ") + " "

	// Deterministic: sort names and return earliest positional match.
	names := make([]string, 0, len(nameCIK))
	for name := range nameCIK {
		names = append(names, name)
	}
	sort.Strings(names)

	var bestName *match
	for _, name := range names {
		idx := strings.Index(tokenStr, " "+name+" ")
		if idx >= 0 {
			if bestName == nil || idx < bestName.pos {
				bestName = &match{cik: nameCIK[name], pos: idx}
			}
		}
	}
	if bestName != nil {
		return bestName.cik
	}
	return ""
}
