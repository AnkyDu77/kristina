package main

// Веб-поиск — Parallel Search MCP (https://search.parallel.ai/mcp): тот
// же бесплатный бесключевой поиск, что у OpenMuse. Тащить MCP SDK ради
// одного сервера незачем: протокол — JSON-RPC поверх POST, три вызова
// (initialize → notifications/initialized → tools/call), сессия в
// заголовке Mcp-Session-Id. Ответ сервер отдаёт обычным JSON, но по
// спецификации может и SSE-потоком — разбираем оба.
//
// В Parallel уходят запрос и цель поиска (а для web_fetch — адрес
// страницы). Это внешний сервис: свои данные модель туда класть не должна,
// об этом — в описании инструмента.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	mcpProtocolVersion = "2025-06-18"
	searchTimeout      = 45 * time.Second
	searchMaxBody      = 1 << 20
)

var parallelMCPURL = "https://search.parallel.ai/mcp"

type searchClient struct {
	url string
	hc  *http.Client
	ids atomic.Int64

	mu      sync.Mutex
	session string // Mcp-Session-Id; пусто — надо инициализироваться
}

func newSearchClient() *searchClient {
	return &searchClient{url: parallelMCPURL, hc: &http.Client{
		Timeout: searchTimeout,
		// Редирект с MCP-эндпоинта — повод насторожиться, а не идти следом
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
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

	raw, err := c.callTool(ctx, name, args)
	if errors.Is(err, errMCPSession) {
		// Сервер забыл сессию (рестарт, истекла) — одна попытка с новой
		c.mu.Lock()
		c.session = ""
		c.mu.Unlock()
		raw, err = c.callTool(ctx, name, args)
	}
	if err != nil {
		return webResults{}, fmt.Errorf("поиск Parallel: %w", err)
	}

	var res struct {
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return webResults{}, fmt.Errorf("поиск Parallel: битый ответ: %w", err)
	}
	text := ""
	for _, b := range res.Content {
		if b.Type == "text" {
			text = b.Text
			break
		}
	}
	if res.IsError {
		return webResults{}, fmt.Errorf("поиск Parallel: %s", truncRunes(text, 300))
	}
	payload := []byte(res.StructuredContent)
	if len(payload) == 0 || string(payload) == "null" {
		payload = []byte(text)
	}
	var out webResults
	if err := json.Unmarshal(payload, &out); err != nil {
		return webResults{}, fmt.Errorf("поиск Parallel: неожиданный формат: %w", err)
	}
	return out, nil
}

var errMCPSession = errors.New("MCP-сессия недействительна")

func (c *searchClient) callTool(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	if err := c.ensureSession(ctx); err != nil {
		return nil, err
	}
	return c.rpc(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
}

func (c *searchClient) ensureSession(ctx context.Context) error {
	c.mu.Lock()
	ok := c.session != ""
	c.mu.Unlock()
	if ok {
		return nil
	}
	if _, err := c.rpc(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "kristina", "version": "1"},
	}); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	return c.notify(ctx, "notifications/initialized")
}

// rpc — запрос JSON-RPC; возвращает result.
func (c *searchClient) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.ids.Add(1)
	resp, err := c.send(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if method == "initialize" {
		if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
			c.mu.Lock()
			c.session = s
			c.mu.Unlock()
		}
	}

	var msg struct {
		ID     int64           `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	body := io.LimitReader(resp.Body, searchMaxBody)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		data, err := sseResponse(body, id)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	if err := json.NewDecoder(body).Decode(&msg); err != nil {
		return nil, fmt.Errorf("%s: битый ответ: %w", method, err)
	}
	if msg.Error != nil {
		return nil, fmt.Errorf("%s: %s", method, msg.Error.Message)
	}
	return msg.Result, nil
}

func (c *searchClient) notify(ctx context.Context, method string) error {
	resp, err := c.send(ctx, map[string]any{"jsonrpc": "2.0", "method": method})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *searchClient) send(ctx context.Context, payload any) (*http.Response, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
	c.mu.Lock()
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	c.mu.Unlock()

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound && req.Header.Get("Mcp-Session-Id") != "":
		resp.Body.Close()
		return nil, errMCPSession
	case resp.StatusCode >= 300:
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("лимит бесплатного поиска, попробуй позже")
		}
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return resp, nil
}

// sseResponse достаёт из SSE-потока сообщение-ответ на запрос id
// (сервер вправе прислать перед ним уведомления).
func sseResponse(r io.Reader, id int64) ([]byte, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), searchMaxBody)
	var data bytes.Buffer
	flush := func() []byte {
		defer data.Reset()
		var probe struct {
			ID *int64 `json:"id"`
		}
		if json.Unmarshal(data.Bytes(), &probe) == nil && probe.ID != nil && *probe.ID == id {
			return append([]byte(nil), data.Bytes()...)
		}
		return nil
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if msg := flush(); msg != nil {
				return msg, nil
			}
			continue
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			data.WriteString(strings.TrimPrefix(v, " "))
		}
	}
	if msg := flush(); msg != nil {
		return msg, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("в SSE-потоке нет ответа на запрос %d", id)
}
