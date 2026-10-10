package main

// Клиент «мозга» — любой OpenAI-совместимый /v1/chat/completions:
// llmster на GPU Кристины (по умолчанию), lmify, vLLM, облачный
// провайдер open-weight моделей. Стриминг не нужен: ответ всё равно
// уходит в телеграм целиком.
//
// База передаётся в каждый вызов, а не задаётся раз и навсегда: у GPU,
// поднятой терраформом, на каждом подъёме новый IP.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls — ход модели «вызвать инструменты» (role=assistant)
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
	// ToolCallID — ответ инструмента на конкретный вызов (role=tool)
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function toolFunc `json:"function"`
}

type toolFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON строкой — так в протоколе OpenAI
}

// toolSpec — описание инструмента для модели.
type toolSpec struct {
	Type     string       `json:"type"`
	Function toolSpecFunc `json:"function"`
}

type toolSpecFunc struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type llmClient struct {
	key string
	// model — ключ модели. Пусто = спросить у сервера (discoverModel):
	// ключ LM Studio не совпадает с именем папки в S3, гадать его не надо.
	model string
	hc    *http.Client

	mu         sync.Mutex
	discovered map[string]string // база → найденный ключ модели
}

func newLLMClient(key, model string) *llmClient {
	return &llmClient{
		key: key, model: model,
		hc:         &http.Client{Timeout: 5 * time.Minute},
		discovered: map[string]string{},
	}
}

// chat — один ход модели. tools == nil — инструменты не предлагаем
// (речь для кружка, последний шаг агента): модель обязана ответить текстом.
func (c *llmClient) chat(ctx context.Context, base string, msgs []chatMsg, tools []toolSpec) (chatMsg, error) {
	base = strings.TrimRight(base, "/")
	model, err := c.modelFor(ctx, base)
	if err != nil {
		return chatMsg{}, err
	}
	body := map[string]any{
		"model":       model,
		"messages":    msgs,
		"temperature": 0.7,
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	return c.post(ctx, base, body, len(tools) > 0)
}

// modelFor — заданная модель или найденная на сервере. Найденную
// запоминаем по базе: новая машина — новая база — спросим заново.
func (c *llmClient) modelFor(ctx context.Context, base string) (string, error) {
	if c.model != "" {
		return c.model, nil
	}
	c.mu.Lock()
	m := c.discovered[base]
	c.mu.Unlock()
	if m != "" {
		return m, nil
	}
	m, err := c.discoverModel(ctx, base)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.discovered[base] = m
	c.mu.Unlock()
	return m, nil
}

// discoverModel спрашивает нативный /api/v0/models LM Studio — там есть
// тип и состояние, так что можно взять именно загруженную LLM, а не
// встроенную embedding-модель. Не LM Studio — откатываемся на /v1/models.
func (c *llmClient) discoverModel(ctx context.Context, base string) (string, error) {
	var native struct {
		Data []struct {
			ID    string `json:"id"`
			Type  string `json:"type"`
			State string `json:"state"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, strings.TrimSuffix(base, "/v1")+"/api/v0/models", &native); err == nil {
		first := ""
		for _, d := range native.Data {
			// vlm — мультимодальная (Qwen3.6 с mmproj): для нас та же LLM
			if d.Type != "llm" && d.Type != "vlm" {
				continue
			}
			if d.State == "loaded" {
				return d.ID, nil
			}
			if first == "" {
				first = d.ID
			}
		}
		if first != "" {
			return first, nil
		}
	}
	var compat struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, base+"/models", &compat); err != nil {
		return "", fmt.Errorf("LLM: не удалось узнать модель: %w", err)
	}
	for _, d := range compat.Data {
		if !strings.Contains(strings.ToLower(d.ID), "embed") {
			return d.ID, nil
		}
	}
	return "", fmt.Errorf("LLM: на %s нет ни одной модели", base)
}

func (c *llmClient) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func (c *llmClient) post(ctx context.Context, base string, body map[string]any, withTools bool) (chatMsg, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return chatMsg{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return chatMsg{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return chatMsg{}, fmt.Errorf("LLM недоступна: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return chatMsg{}, fmt.Errorf("LLM %s: %s", resp.Status, strings.TrimSpace(string(data[:min(len(data), 300)])))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content   string     `json:"content"`
				ToolCalls []toolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return chatMsg{}, fmt.Errorf("LLM: битый ответ: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return chatMsg{}, fmt.Errorf("LLM: пустой ответ")
	}
	m := parsed.Choices[0].Message
	out := chatMsg{Role: "assistant", Content: stripThinking(m.Content), ToolCalls: m.ToolCalls}
	if len(out.ToolCalls) == 0 && withTools {
		out.Content, out.ToolCalls = textToolCalls(out.Content)
	}
	for i := range out.ToolCalls {
		if out.ToolCalls[i].ID == "" {
			out.ToolCalls[i].ID = fmt.Sprintf("call_%d", i)
		}
		out.ToolCalls[i].Type = "function"
	}
	return out, nil
}

// toolCallRe — вызов инструмента, оставшийся текстом. Qwen пишет его
// тегами <tool_call>{...}</tool_call>; если сервер не распознал их (не тот
// шаблон чата, модель сбилась), вызов приходит в content, а не в tool_calls.
var toolCallRe = regexp.MustCompile(`(?s)<tool_call>\s*(\{.*?\})\s*</tool_call>`)

// textToolCalls достаёт такие вызовы из текста и возвращает текст без них.
func textToolCalls(content string) (string, []toolCall) {
	var calls []toolCall
	for _, m := range toolCallRe.FindAllStringSubmatch(content, -1) {
		var c struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal([]byte(m[1]), &c) != nil || c.Name == "" {
			continue
		}
		args := string(c.Arguments)
		// arguments бывают и объектом, и уже строкой с JSON внутри
		var s string
		if json.Unmarshal(c.Arguments, &s) == nil {
			args = s
		}
		if args == "" {
			args = "{}"
		}
		calls = append(calls, toolCall{Function: toolFunc{Name: c.Name, Arguments: args}})
	}
	if len(calls) == 0 {
		return content, nil
	}
	return strings.TrimSpace(toolCallRe.ReplaceAllString(content, "")), calls
}
