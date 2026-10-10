package main

// Минимальный MCP-клиент (Streamable HTTP): им ходим в Parallel Search,
// в браузер на GPU-машине (Playwright MCP) и во внешние серверы из
// KRISTINA_MCP_SERVERS. Тащить SDK ради этого незачем: протокол — JSON-RPC
// поверх POST, три вызова (initialize → notifications/initialized →
// tools/list | tools/call), сессия в заголовке Mcp-Session-Id. Ответ сервер
// отдаёт обычным JSON или SSE-потоком — разбираем оба.

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

const mcpProtocolVersion = "2025-06-18"

type mcpClient struct {
	url     string
	key     string // Bearer; пусто — без авторизации
	maxBody int64
	hc      *http.Client
	ids     atomic.Int64

	mu      sync.Mutex
	session string // Mcp-Session-Id; пусто — надо инициализироваться
}

func newMCPClient(url, key string, timeout time.Duration, maxBody int64) *mcpClient {
	return &mcpClient{url: url, key: key, maxBody: maxBody, hc: &http.Client{
		Timeout: timeout,
		// Редирект с MCP-эндпоинта — повод насторожиться, а не идти следом
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// mcpTool — описание инструмента из tools/list.
type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations struct {
		ReadOnlyHint *bool `json:"readOnlyHint"`
	} `json:"annotations"`
}

type mcpContent struct {
	Type     string `json:"type"` // text | image | …
	Text     string `json:"text"`
	Data     string `json:"data"` // base64 для image
	MimeType string `json:"mimeType"`
}

type mcpResult struct {
	IsError           bool            `json:"isError"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	Content           []mcpContent    `json:"content"`
}

// text — все текстовые блоки результата подряд.
func (r *mcpResult) text() string {
	var parts []string
	for _, c := range r.Content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func (c *mcpClient) listTools(ctx context.Context) ([]mcpTool, error) {
	raw, err := c.withSession(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []mcpTool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	return out.Tools, nil
}

// callTool — tools/call. isError в результате ошибкой не считается: это
// ответ инструмента, и модели полезно его увидеть.
func (c *mcpClient) callTool(ctx context.Context, name string, args any) (*mcpResult, error) {
	raw, err := c.withSession(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, err
	}
	var res mcpResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("tools/call %s: битый ответ: %w", name, err)
	}
	return &res, nil
}

var errMCPSession = errors.New("MCP-сессия недействительна")

// withSession — запрос в рамках сессии; если сервер её забыл (рестарт,
// истекла) — одна попытка с новой.
func (c *mcpClient) withSession(ctx context.Context, method string, params any) (json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		if err := c.ensureSession(ctx); err != nil {
			return nil, err
		}
		raw, err := c.rpc(ctx, method, params)
		if errors.Is(err, errMCPSession) && attempt == 0 {
			c.mu.Lock()
			c.session = ""
			c.mu.Unlock()
			continue
		}
		return raw, err
	}
}

func (c *mcpClient) ensureSession(ctx context.Context) error {
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
func (c *mcpClient) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
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
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	body := io.LimitReader(resp.Body, c.maxBody)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		data, err := sseResponse(body, id, c.maxBody)
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

func (c *mcpClient) notify(ctx context.Context, method string) error {
	resp, err := c.send(ctx, map[string]any{"jsonrpc": "2.0", "method": method})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *mcpClient) send(ctx context.Context, payload any) (*http.Response, error) {
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
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
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
			return nil, fmt.Errorf("лимит запросов, попробуй позже")
		}
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return resp, nil
}

// sseResponse достаёт из SSE-потока сообщение-ответ на запрос id
// (сервер вправе прислать перед ним уведомления).
func sseResponse(r io.Reader, id int64, maxBody int64) ([]byte, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), int(maxBody))
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
