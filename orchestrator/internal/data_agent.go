package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

	// Check tickers: tokenize query and do direct map lookups — O(#tokens).
	// Deterministic: returns the first ticker found by position in query.
	tokens := strings.FieldsFunc(upper, func(r rune) bool {
		return (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	})
	for _, token := range tokens {
		if cik, ok := c.byTicker[token]; ok {
			return cik
		}
	}

	// Check company names — earliest positional match for determinism.
	bestPos := -1
	bestName := ""
	bestCIK := ""
	for name, cik := range c.byName {
		pos := strings.Index(upper, name)
		if pos == -1 {
			continue
		}
		if bestPos == -1 || pos < bestPos || (pos == bestPos && name < bestName) {
			bestPos = pos
			bestName = name
			bestCIK = cik
		}
	}
	return bestCIK
}

func (c *cikCache) needsRefresh() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.byTicker == nil || time.Since(c.fetched) > cikCacheTTL
}

// corporateSuffixes are stripped from SEC titles for fuzzy name matching.
var corporateSuffixes = []string{
	" INC.", " INC", " CORP.", " CORP", " CO.", " CO",
	" LTD.", " LTD", " LP", " L.P.", " LLC", " PLC",
	" HOLDINGS", " GROUP", " TECHNOLOGIES", " TECHNOLOGY",
}

// normalizeName strips corporate suffixes and punctuation for fuzzy matching.
func normalizeName(name string) string {
	n := strings.ToUpper(strings.TrimSpace(name))
	for _, suffix := range corporateSuffixes {
		n = strings.TrimSuffix(n, suffix)
	}
	// Strip remaining punctuation
	n = strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == ' ' {
			return r
		}
		return -1
	}, n)
	return strings.TrimSpace(n)
}

func (c *cikCache) populate(ctx context.Context, httpClient *http.Client, userAgent string) error {
	// Quick check under read lock — avoid unnecessary work
	c.mu.RLock()
	if c.byTicker != nil && time.Since(c.fetched) <= cikCacheTTL {
		c.mu.RUnlock()
		return nil
	}
	c.mu.RUnlock()

	ua := strings.TrimSpace(userAgent)
	if ua == "" {
		return fmt.Errorf("SEC EDGAR lookup requires SEC_EDGAR_USER_AGENT to be set")
	}

	// Fetch and parse outside the write lock to avoid blocking lookups
	req, err := http.NewRequestWithContext(ctx, "GET", "https://www.sec.gov/files/company_tickers.json", nil)
	if err != nil {
		return err
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
		CIK    int64  `json:"cik_str"`
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
		// Store both the full title and normalized (suffix-stripped) form
		fullName := strings.ToUpper(e.Title)
		if _, exists := byName[fullName]; !exists {
			byName[fullName] = cik
		}
		norm := normalizeName(e.Title)
		if norm != fullName {
			if _, exists := byName[norm]; !exists {
				byName[norm] = cik
			}
		}
	}

	// Swap maps in under write lock — minimal lock duration
	c.mu.Lock()
	c.byTicker = byTicker
	c.byName = byName
	c.fetched = time.Now()
	c.mu.Unlock()

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
	edgarUA  string
	http     *http.Client
	cikCache *cikCache
}

func NewDataAgent(gemini *GeminiClient, store *Store, edgarUA string) *DataAgent {
	return &DataAgent{
		gemini:   gemini,
		store:    store,
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
	var failedQueries []string
	var totalUsage TokenUsage

	for _, query := range queries {
		found := false

		// Try SEC EDGAR if the query mentions a known company
		if cik := a.cikCache.lookup(query); cik != "" {
			data, err := a.fetchEDGAR(ctx, cik, query)
			if err != nil {
				slog.Warn("EDGAR fetch failed", "query", query, "error", err)
			} else {
				rawChunks = append(rawChunks, fmt.Sprintf("[SEC EDGAR] %s:\n%s", query, data))
				found = true
			}
		}

		// Web search for broader context (via Gemini Google Search grounding)
		results, searchUsage, err := a.searchWeb(ctx, query)
		totalUsage = totalUsage.Add(searchUsage)
		if err != nil {
			slog.Warn("web search failed", "query", query, "error", err)
		} else if results != "" {
			rawChunks = append(rawChunks, fmt.Sprintf("[Web Search] '%s':\n%s", query, results))
			found = true
		}

		if !found {
			failedQueries = append(failedQueries, query)
		}
	}

	if len(rawChunks) == 0 {
		slog.Warn("data agent: no data collected", "job_id", job.JobID)
		// Record that no data was found so the orchestrator doesn't loop.
		noDataFact := Fact{
			Key:    "data_unavailable",
			Value:  fmt.Sprintf("No data could be retrieved for: %s. Web search and SEC EDGAR returned no results.", strings.Join(queries, ", ")),
			Source: "data_agent",
		}
		allFacts := append(job.CollectedFacts, noDataFact)
		summary := fmt.Sprintf("Data agent completed. Queries attempted: %d. All queries returned no results. Queries: %s",
			len(queries), strings.Join(queries, "; "))
		return totalUsage, a.store.UpdateJob(ctx, job.JobID, job.SessionID, []firestore.Update{
			{Path: "collected_facts", Value: allFacts},
			{Path: "missing_queries", Value: []string{}},
			{Path: "last_agent_summary", Value: summary},
		})
	}

	// LLM-extract structured facts from raw data
	combined := strings.Join(rawChunks, "\n\n---\n\n")
	if len(combined) > 30000 {
		combined = combined[:30000] + "\n...[truncated]"
	}
	prompt := fmt.Sprintf("Instructions: %s\n\nRaw data:\n%s", instructions, combined)

	var facts []Fact
	extractUsage, err := a.gemini.GenerateJSON(ctx, factExtractionPrompt, prompt, &facts)
	totalUsage = totalUsage.Add(extractUsage)
	if err != nil {
		return totalUsage, fmt.Errorf("fact extraction: %w", err)
	}

	// If LLM extracted 0 new facts, record that explicitly so the orchestrator
	// knows data collection yielded nothing and doesn't loop.
	if len(facts) == 0 && len(failedQueries) > 0 {
		facts = append(facts, Fact{
			Key:    "data_unavailable",
			Value:  fmt.Sprintf("Could not find relevant data for: %s", strings.Join(failedQueries, ", ")),
			Source: "data_agent",
		})
	}

	allFacts := append(job.CollectedFacts, facts...)
	slog.Info("data agent: done", "job_id", job.JobID, "new_facts", len(facts), "total_facts", len(allFacts))

	var summaryParts []string
	summaryParts = append(summaryParts, fmt.Sprintf("Data agent completed. Queries attempted: %d. New facts extracted: %d (total: %d).",
		len(queries), len(facts), len(allFacts)))
	if len(failedQueries) > 0 {
		summaryParts = append(summaryParts, fmt.Sprintf("Failed queries: %s.", strings.Join(failedQueries, "; ")))
	}
	for _, f := range facts {
		summaryParts = append(summaryParts, fmt.Sprintf("- %s: %s", f.Key, f.Value))
	}
	summary := strings.Join(summaryParts, "\n")
	summary = truncateRunes(summary, 1000)

	return totalUsage, a.store.UpdateJob(ctx, job.JobID, job.SessionID, []firestore.Update{
		{Path: "collected_facts", Value: allFacts},
		{Path: "missing_queries", Value: []string{}},
		{Path: "last_agent_summary", Value: summary},
	})
}

// --- Web Search (Gemini Google Search grounding) ---

func (a *DataAgent) searchWeb(ctx context.Context, query string) (string, TokenUsage, error) {
	results, summary, usage, err := a.gemini.SearchWeb(ctx, query)
	if err != nil {
		return "", usage, err
	}

	var sb strings.Builder
	if summary != "" {
		sb.WriteString(summary)
		sb.WriteString("\n\nSources:\n")
	}
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("%d. %s\n   Source: %s\n", i+1, r.Title, r.URL))
	}
	return sb.String(), usage, nil
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
