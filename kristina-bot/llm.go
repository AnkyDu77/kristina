package main

// Клиент «мозга» — любой OpenAI-совместимый /v1/chat/completions:
// lmify (LM Studio), vLLM, llama.cpp server, облачный провайдер
// open-weight моделей. Стриминг не нужен: ответ всё равно сначала
// целиком уходит в TTS.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type llmClient struct {
	base, key, model string
	// jsonSchema — просить у сервера structured output. Не все серверы и
	// модели его умеют; на 400 откатываемся на обычный запрос, а формат
	// держит промпт + терпимый разбор в parseReply.
	jsonSchema bool
	hc         *http.Client
}

func newLLMClient(base, key, model string, jsonSchema bool) *llmClient {
	return &llmClient{
		base: strings.TrimRight(base, "/"), key: key, model: model,
		jsonSchema: jsonSchema,
		hc:         &http.Client{Timeout: 5 * time.Minute},
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

func (c *llmClient) complete(ctx context.Context, msgs []chatMsg) (string, error) {
	body := map[string]any{
		"model":       c.model,
		"messages":    msgs,
		"temperature": 0.7,
	}
	if c.jsonSchema {
		body["response_format"] = replySchema
		out, status, err := c.post(ctx, body)
		if status != http.StatusBadRequest {
			return out, err
		}
		delete(body, "response_format")
	}
	out, _, err := c.post(ctx, body)
	return out, err
}

func (c *llmClient) post(ctx context.Context, body map[string]any) (string, int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/chat/completions", bytes.NewReader(raw))
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
