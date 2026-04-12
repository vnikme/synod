package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
)

// cikCache holds the dynamically fetched SEC company→CIK mapping.
// Populated lazily from https://www.sec.gov/files/company_tickers.json
type cikCache struct {
	mu       sync.RWMutex
	byTicker map[string]string // "AAPL" -> "0000320193"
	byName   map[string]string // "APPLE INC" -> "0000320193"
	fetched  time.Time
}

const cikCacheTTL = 24 * time.Hour

func (c *cikCache) lookup(query string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	upper := strings.ToUpper(query)

	// Check tickers — look for exact word boundary match
	for ticker, cik := range c.byTicker {
		if strings.Contains(" "+upper+" ", " "+ticker+" ") {
			return cik
		}
	}

	// Check company names — substring match
	for name, cik := range c.byName {
		if strings.Contains(upper, name) {
			return cik
		}
	}
	return ""
}

func (c *cikCache) needsRefresh() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.byTicker == nil || time.Since(c.fetched) > cikCacheTTL
}

func (c *cikCache) populate(ctx context.Context, httpClient *http.Client, userAgent string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check after acquiring write lock
	if c.byTicker != nil && time.Since(c.fetched) <= cikCacheTTL {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, "GET", "https://www.sec.gov/files/company_tickers.json", nil)
	if err != nil {
		return err
	}
	ua := userAgent
	if ua == "" {
		ua = "Synod/1.0 (synod@example.com)"
	}
	req.Header.Set("User-Agent", ua)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch company_tickers.json: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("SEC company_tickers returned %d", resp.StatusCode)
	}

	// Response format: {"0": {"cik_str": 320193, "ticker": "AAPL", "title": "Apple Inc"}, ...}
	var entries map[string]struct {
		CIK    int    `json:"cik_str"`
		Ticker string `json:"ticker"`
		Title  string `json:"title"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return fmt.Errorf("parse company_tickers.json: %w", err)
	}

	byTicker := make(map[string]string, len(entries))
	byName := make(map[string]string, len(entries))
	for _, e := range entries {
		cik := fmt.Sprintf("%010d", e.CIK)
		byTicker[strings.ToUpper(e.Ticker)] = cik
		// Use first word(s) of title for name matching
		name := strings.ToUpper(e.Title)
		if _, exists := byName[name]; !exists {
			byName[name] = cik
		}
	}

	c.byTicker = byTicker
	c.byName = byName
	c.fetched = time.Now()
	slog.Info("SEC CIK cache populated", "tickers", len(byTicker), "names", len(byName))
	return nil
}

const factExtractionPrompt = `Extract structured facts from the provided raw data.
Return a JSON array of objects: [{"key": "metric name", "value": "value with units", "source": "data source URL or name"}]
Only extract facts relevant to the given instructions.
Be precise with numbers. Include units and time periods.
Limit to the most important 15-20 facts.`

type DataAgent struct {
	gemini   *GeminiClient
	store    *Store
	cseKey   string
	cseCX    string
	edgarUA  string
	http     *http.Client
	cikCache *cikCache
}

func NewDataAgent(gemini *GeminiClient, store *Store, cseKey, cseCX, edgarUA string) *DataAgent {
	return &DataAgent{
		gemini:   gemini,
		store:    store,
		cseKey:   cseKey,
		cseCX:    cseCX,
		edgarUA:  edgarUA,
		http:     &http.Client{Timeout: 30 * time.Second},
		cikCache: &cikCache{},
	}
}

func (a *DataAgent) Execute(ctx context.Context, job *Job, instructions string, queries []string) (TokenUsage, error) {
	slog.Info("data agent: starting", "job_id", job.JobID, "num_queries", len(queries))

	// Ensure CIK cache is populated
	if a.cikCache.needsRefresh() {
		if err := a.cikCache.populate(ctx, a.http, a.edgarUA); err != nil {
			slog.Warn("SEC CIK cache refresh failed, EDGAR lookups may miss", "error", err)
		}
	}

	var rawChunks []string

	for _, query := range queries {
		// Try SEC EDGAR if the query mentions a known company
		if cik := a.cikCache.lookup(query); cik != "" {
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
		return usage, fmt.Errorf("fact extraction: %w", err)
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
