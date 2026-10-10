package main

// Веб-поиск — Parallel Search MCP (https://search.parallel.ai/mcp): тот
// же бесплатный бесключевой поиск, что у OpenMuse.
//
// В Parallel уходят запрос и цель поиска (а для web_fetch — адрес
// страницы). Это внешний сервис: свои данные модель туда класть не должна,
// об этом — в описании инструмента.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

const (
	searchTimeout = 45 * time.Second
	searchMaxBody = 1 << 20
)

var parallelMCPURL = "https://search.parallel.ai/mcp"

type searchClient struct{ mcp *mcpClient }

func newSearchClient() *searchClient {
	return &searchClient{mcp: newMCPClient(parallelMCPURL, "", searchTimeout, searchMaxBody)}
}

type webSource struct {
	URL         string   `json:"url"`
	Title       string   `json:"title"`
	PublishDate string   `json:"publish_date"`
	Excerpts    []string `json:"excerpts"`
	FullContent string   `json:"full_content"`
}

type webResults struct {
	Results  []webSource       `json:"results"`
	Warnings []json.RawMessage `json:"warnings"`
	Errors   []json.RawMessage `json:"errors"`
}

// search — web_search: цель + 1–5 коротких запросов.
func (c *searchClient) search(ctx context.Context, sessionID, objective string, queries []string) (webResults, error) {
	return c.tool(ctx, "web_search", map[string]any{
		"objective": objective, "search_queries": queries, "session_id": sessionID,
	})
}

// fetch — web_fetch: выдержки страницы под цель. Запасной путь для
// read_page, когда сами страницу прочесть не смогли (JS, PDF, защита).
func (c *searchClient) fetch(ctx context.Context, sessionID, url, objective string) (webResults, error) {
	args := map[string]any{"urls": []string{url}, "session_id": sessionID}
	if objective != "" {
		args["objective"] = objective
	}
	return c.tool(ctx, "web_fetch", args)
}

func (c *searchClient) tool(ctx context.Context, name string, args map[string]any) (webResults, error) {
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	res, err := c.mcp.callTool(ctx, name, args)
	if err != nil {
		return webResults{}, fmt.Errorf("поиск Parallel: %w", err)
	}
	if res.IsError {
		return webResults{}, fmt.Errorf("поиск Parallel: %s", truncRunes(res.text(), 300))
	}
	payload := []byte(res.StructuredContent)
	if len(payload) == 0 || string(payload) == "null" {
		payload = []byte(res.text())
	}
	var out webResults
	if err := json.Unmarshal(payload, &out); err != nil {
		return webResults{}, fmt.Errorf("поиск Parallel: неожиданный формат: %w", err)
	}
	return out, nil
}
