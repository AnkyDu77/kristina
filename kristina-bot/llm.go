package main

// Клиент «мозга» — любой OpenAI-совместимый /v1/chat/completions:
// llmster на GPU Кристины (по умолчанию), lmify, vLLM, облачный
// провайдер open-weight моделей. Стриминг не нужен: ответ всё равно
// сначала целиком уходит в TTS.
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
	"strings"
	"sync"
	"time"
)

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type llmClient struct {
	key string
	// model — ключ модели. Пусто = спросить у сервера (discoverModel):
	// ключ LM Studio не совпадает с именем папки в S3, гадать его не надо.
	model string
	// jsonSchema — просить у сервера structured output. Не все серверы и
	// модели его умеют; на 400 откатываемся на обычный запрос, а формат
	// держит промпт + терпимый разбор в parseReply.
	jsonSchema bool
	hc         *http.Client

	mu         sync.Mutex
	discovered map[string]string // база → найденный ключ модели
}

func newLLMClient(key, model string, jsonSchema bool) *llmClient {
	return &llmClient{
		key: key, model: model, jsonSchema: jsonSchema,
		hc:         &http.Client{Timeout: 5 * time.Minute},
		discovered: map[string]string{},
	}
}

// replySchema — то же, что описано в persona.go словами.
var replySchema = map[string]any{
	"type": "json_schema",
	"json_schema": map[string]any{
		"name":   "kristina_reply",
		"strict": true,
		"schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"speech": map[string]any{"type": "string"},
				"text":   map[string]any{"type": "string"},
			},
			"required":             []string{"speech", "text"},
			"additionalProperties": false,
		},
	},
}

func (c *llmClient) complete(ctx context.Context, base string, msgs []chatMsg) (string, error) {
	base = strings.TrimRight(base, "/")
	model, err := c.modelFor(ctx, base)
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"model":       model,
		"messages":    msgs,
		"temperature": 0.7,
	}
	if c.jsonSchema {
		body["response_format"] = replySchema
		out, status, err := c.post(ctx, base, body)
		if status != http.StatusBadRequest {
			return out, err
		}
		delete(body, "response_format")
	}
	out, _, err := c.post(ctx, base, body)
	return out, err
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
			if d.Type != "llm" {
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

func (c *llmClient) post(ctx context.Context, base string, body map[string]any) (string, int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("LLM недоступна: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode, fmt.Errorf("LLM %s: %s", resp.Status, strings.TrimSpace(string(data[:min(len(data), 300)])))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", resp.StatusCode, fmt.Errorf("LLM: битый ответ: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", resp.StatusCode, fmt.Errorf("LLM: пустой ответ")
	}
	return parsed.Choices[0].Message.Content, resp.StatusCode, nil
}
