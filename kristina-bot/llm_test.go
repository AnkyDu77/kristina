package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	c := newLLMClient("k", "", true)
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

	got, err := newLLMClient("", "", true).modelFor(context.Background(), srv.URL+"/v1")
	if err != nil || got != "llama-3" {
		t.Fatalf("got %q, %v", got, err)
	}
}
