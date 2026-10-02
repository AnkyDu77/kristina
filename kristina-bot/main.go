// kristina-bot — Telegram-бот Кристины: сообщение → LLM → голос
// (Qwen3-TTS) → кружок (MuseTalk) на GPU VPS, которую бот сам поднимает
// терраформом и гасит по простою.
package main

import (
	"bufio"
	"context"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	loadDotEnv(".env")
	token := mustEnv("KRISTINA_TG_BOT_TOKEN")
	allowed := parseIDs(mustEnv("KRISTINA_ALLOWED_TGIDS"))
	if len(allowed) == 0 {
		log.Fatal("KRISTINA_ALLOWED_TGIDS: ни одного tg_id — бот личный, без списка он никому не ответит")
	}

	llm := newLLMClient(
		mustEnv("KRISTINA_LLM_BASE"),
		os.Getenv("KRISTINA_LLM_KEY"),
		mustEnv("KRISTINA_LLM_MODEL"),
		envStr("KRISTINA_LLM_JSON_SCHEMA", "1") != "0",
	)
	media := newMediaClient(mustEnv("KRISTINA_MEDIA_KEY"))

	cfg := botConfig{
		allowed:      allowed,
		dataDir:      envStr("KRISTINA_DATA_DIR", "data"),
		avatarPath:   envStr("KRISTINA_AVATAR", "../assets/idle.mp4"),
		language:     envStr("KRISTINA_LANGUAGE", "Russian"),
		maxSpeech:    envInt("KRISTINA_MAX_SPEECH", 600),
		historyTurns: envInt("KRISTINA_HISTORY_TURNS", 12),
		circleSide:   envInt("KRISTINA_CIRCLE_SIDE", 512),
		staticBase:   strings.TrimRight(os.Getenv("KRISTINA_MEDIA_BASE"), "/"),
	}
	if _, err := os.Stat(cfg.avatarPath); err != nil {
		log.Fatalf("KRISTINA_AVATAR: %v", err)
	}

	// Режим GPU: статический (машина поднята руками, адрес в
	// KRISTINA_MEDIA_BASE) или managed (terraform по требованию)
	var gpu *gpuManager
	if cfg.staticBase == "" {
		tfDir := envStr("KRISTINA_TF_DIR", "../selectel")
		tfBin, err := exec.LookPath(envStr("KRISTINA_TF_BIN", "terraform"))
		if err != nil {
			log.Fatalf("terraform не найден (%v): поставь его или задай KRISTINA_MEDIA_BASE", err)
		}
		idle, err := time.ParseDuration(envStr("KRISTINA_GPU_IDLE", "20m"))
		if err != nil {
			log.Fatalf("KRISTINA_GPU_IDLE: %v", err)
		}
		gpu = newGPUManager(tfBin, tfDir, idle)
		gpu.syncFromState()
		log.Printf("gpu: managed-режим, %s, авто-стоп после %s простоя", tfDir, idle)
	} else {
		log.Printf("gpu: статический режим, %s", cfg.staticBase)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	b := newBot(cfg, newTGAPI(token), llm, media, gpu)
	if err := b.run(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
	if gpu != nil && gpu.status().State == "running" {
		// Машину не гасим: бот мог уйти на минутный рестарт. Подхватим её
		// из state при старте, а если не вернёмся — она так и будет
		// тикать, поэтому говорим об этом громко
		log.Printf("gpu: бот остановлен, а GPU VPS работает — погаси её (terraform destroy в %s), если бот не вернётся", gpu.tfDir)
	}
}

// loadDotEnv подхватывает KEY=VALUE из .env рядом с бинарём (без перезаписи
// уже заданных переменных окружения) — как в lmify-ui.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
	log.Printf("config: загружен %s", path)
}

func mustEnv(k string) string {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		log.Fatalf("%s не задан (см. .env.example)", k)
	}
	return v
}

func envStr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("%s: %v", k, err)
	}
	return n
}

func parseIDs(csv string) map[int64]bool {
	ids := map[int64]bool{}
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			log.Fatalf("tg_id %q: %v", s, err)
		}
		ids[id] = true
	}
	return ids
}
