package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiscoverModelPrefersLoadedLLM(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Как за nginx на GPU: /llm/api/v0/models
		if r.URL.Path != "/llm/api/v0/models" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{
			{"id": "text-embedding-nomic-embed-text-v1.5", "type": "embeddings", "state": "loaded"},
			{"id": "qwen3.5-9b", "type": "llm", "state": "not-loaded"},
			{"id": "openai/gpt-oss-20b", "type": "llm", "state": "loaded"},
		}})
	}))
	defer srv.Close()

	c := newLLMClient("k", "")
	got, err := c.modelFor(context.Background(), srv.URL+"/llm/v1")
	if err != nil || got != "openai/gpt-oss-20b" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestDiscoverModelFallsBackToOpenAIList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r) // не LM Studio — нативного API нет
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{
			{"id": "bge-embed"}, {"id": "llama-3"},
		}})
	}))
	defer srv.Close()

	got, err := newLLMClient("", "").modelFor(context.Background(), srv.URL+"/v1")
	if err != nil || got != "llama-3" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestTextToolCalls(t *testing.T) {
	content := "Сейчас поищу.\n<tool_call>\n{\"name\": \"web_search\", \"arguments\": {\"objective\": \"go\", \"search_queries\": [\"go release\"]}}\n</tool_call>"
	rest, calls := textToolCalls(content)
	if len(calls) != 1 || calls[0].Function.Name != "web_search" || rest != "Сейчас поищу." {
		t.Fatalf("rest=%q calls=%+v", rest, calls)
	}
	if argStr(json.RawMessage(calls[0].Function.Arguments), "objective") != "go" {
		t.Fatalf("аргументы: %s", calls[0].Function.Arguments)
	}
	if _, calls := textToolCalls("просто текст"); calls != nil {
		t.Fatal("в обычном тексте вызовов нет")
	}
}

// Ровно то, что Кристина прислала в чат вместо работы (11.10.2026)
const qwenXMLCalls = `Отлично! Главная страница page.tsx собрана правильно — импортирует все 7 компонентов. 

Давай проверю:

<tool_call>
<function=read_file>
<parameter=path>
academy-remont/src/components/home/Testimonials.tsx
</parameter>
</function>
</tool_call>
<tool_call>
<function=read_file>
<parameter=path>
academy-remont/src/components/home/CTA.tsx
</parameter>
<parameter=from_line>
3
</parameter>
</function>
</tool_call>
<tool_call>
<function=write_file>
<parameter=path>
a.txt
</parameter>
<parameter=content>
  отступ сохраняется
42
</parameter>
</function>
</tool_call>`

func TestTextToolCallsQwenXML(t *testing.T) {
	rest, calls := textToolCalls(qwenXMLCalls)
	if len(calls) != 3 {
		t.Fatalf("вызовов: %d (%+v)", len(calls), calls)
	}
	if calls[0].Function.Name != "read_file" || argStr(json.RawMessage(calls[0].Function.Arguments), "path") != "academy-remont/src/components/home/Testimonials.tsx" {
		t.Fatalf("первый: %+v", calls[0])
	}
	if got := argStr(json.RawMessage(calls[2].Function.Arguments), "content"); got != "  отступ сохраняется\n42" {
		t.Fatalf("содержимое файла должно дойти как есть: %q", got)
	}
	if strings.Contains(rest, "<") || !strings.HasSuffix(rest, "Давай проверю:") {
		t.Fatalf("текст без разметки: %q", rest)
	}
}

func TestCoerceArgs(t *testing.T) {
	params := object(map[string]any{
		"path":      str(""),
		"content":   str(""),
		"from_line": map[string]any{"type": "integer"},
		"append":    map[string]any{"type": "boolean"},
		"options":   map[string]any{"type": "array"},
	})
	got := coerceArgs(params, json.RawMessage(`{"path":"a","content":"42","from_line":"3","append":"true","options":"[\"a\",\"b\"]"}`))
	var m map[string]any
	_ = json.Unmarshal(got, &m)
	if m["content"] != "42" || m["from_line"] != float64(3) || m["append"] != true || len(m["options"].([]any)) != 2 {
		t.Fatalf("%s", got)
	}
}
