// kristina-bot — Telegram-бот Кристины: сообщение → агент (LLM +
// инструменты: поиск, чтение страниц, память) → ответ текстом, а итог
// крупной задачи — кружком (Qwen3-TTS + MuseTalk). LLM и медиа живут на
// GPU VPS, которую владелец будит, а бот гасит по простою.
package main

import (
	"bufio"
	"context"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
	// Будить и гасить видеокарту — только им: это деньги
	gpuAdmins := parseIDs(mustEnv("KRISTINA_GPU_ADMIN_TGIDS"))
	for id := range gpuAdmins {
		if !allowed[id] {
			log.Fatalf("KRISTINA_GPU_ADMIN_TGIDS: tg_id %d нет в KRISTINA_ALLOWED_TGIDS — его сообщения бот не увидит вовсе", id)
		}
	}

	mediaKey := mustEnv("KRISTINA_MEDIA_KEY")
	media := newMediaClient(mediaKey)

	// «Мозг»: по умолчанию — на той же GPU, под ключом медиа-API.
	// KRISTINA_LLM_BASE — внешний эндпоинт со своим ключом.
	llmBase := strings.TrimRight(os.Getenv("KRISTINA_LLM_BASE"), "/")
	llmKey := mediaKey
	if llmBase != "" {
		llmKey = os.Getenv("KRISTINA_LLM_KEY")
	}
	llm := newLLMClient(llmKey, os.Getenv("KRISTINA_LLM_MODEL"))

	tz, err := time.LoadLocation(envStr("KRISTINA_TZ", "Europe/Moscow"))
	if err != nil {
		log.Fatalf("KRISTINA_TZ: %v", err)
	}
	approvalTimeout, err := time.ParseDuration(envStr("KRISTINA_APPROVAL_TIMEOUT", "10m"))
	if err != nil {
		log.Fatalf("KRISTINA_APPROVAL_TIMEOUT: %v", err)
	}

	cfg := botConfig{
		allowed:      allowed,
		gpuAdmins:    gpuAdmins,
		dataDir:      envStr("KRISTINA_DATA_DIR", "data"),
		avatarPath:   envStr("KRISTINA_AVATAR", "../assets/idle.mp4"),
		language:     envStr("KRISTINA_LANGUAGE", "Russian"),
		maxSpeech:    envInt("KRISTINA_MAX_SPEECH", 600),
		historyTurns: envInt("KRISTINA_HISTORY_TURNS", 12),
		circleSide:   envInt("KRISTINA_CIRCLE_SIDE", 512),
		staticBase:   strings.TrimRight(os.Getenv("KRISTINA_MEDIA_BASE"), "/"),
		llmBase:      llmBase,
		maxSteps:     envInt("KRISTINA_AGENT_MAX_STEPS", 6),
		// 0 — кружки только по /circle
		circleMinTools:  envInt("KRISTINA_CIRCLE_MIN_TOOLS", 3),
		approvalTimeout: approvalTimeout,
		tz:              tz,
		taskMaxSteps:    envInt("KRISTINA_TASK_MAX_STEPS", 25),
		monitorDefault:  envDuration("KRISTINA_MONITOR_DEFAULT", time.Hour),
		monitorMin:      envDuration("KRISTINA_MONITOR_MIN", 10*time.Minute),
		browserEnabled:  envStr("KRISTINA_BROWSER", "1") != "0",
		browserURL:      strings.TrimRight(os.Getenv("KRISTINA_BROWSER_URL"), "/"),
		sandboxNetwork:  envStr("KRISTINA_SANDBOX_NETWORK", "none"),
	}
	if _, err := os.Stat(cfg.avatarPath); err != nil {
		log.Fatalf("KRISTINA_AVATAR: %v", err)
	}

	// История, память, подтверждения, отложенные вопросы. Рядом с голосом
	// в data/ — тот же volume переживает пересоздание контейнера
	dbPath := envStr("KRISTINA_DB", filepath.Join(cfg.dataDir, "kristina.db"))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		log.Fatalf("KRISTINA_DB: %v", err)
	}
	st, err := openStore(dbPath)
	if err != nil {
		log.Fatalf("store %s: %v", dbPath, err)
	}
	defer st.db.Close()
	if n, err := st.expirePending(); err != nil {
		log.Fatalf("store: %v", err)
	} else if n > 0 {
		log.Printf("store: %d подтверждений прошлого процесса — просрочены", n)
	}
	if n, err := st.requeueRunning(); err != nil {
		log.Fatalf("store: %v", err)
	} else if n > 0 {
		log.Printf("store: %d задач прошлого процесса — продолжу с последнего шага", n)
	}
	log.Printf("store: %s", dbPath)

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

	b := newBot(cfg, newTGAPI(token), llm, media, gpu, st)

	// Компьютер: рабочая папка (всегда), песочница (если есть докер),
	// внешние MCP-серверы (если заданы)
	ws, err := openWorkspace(envStr("KRISTINA_WORKSPACE", filepath.Join(cfg.dataDir, "workspace")))
	if err != nil {
		log.Fatalf("KRISTINA_WORKSPACE: %v", err)
	}
	servers, err := parseMCPServers(os.Getenv("KRISTINA_MCP_SERVERS"))
	if err != nil {
		log.Fatal(err)
	}
	b.attach(ws, sandboxFromEnv(ws.dir, cfg.sandboxNetwork), servers)

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

// sandboxFromEnv — песочница, если докер доступен и образ собран.
// KRISTINA_SANDBOX: auto (по умолчанию) | docker (обязательна) | off.
func sandboxFromEnv(wsDir, network string) sandbox {
	mode := envStr("KRISTINA_SANDBOX", "auto")
	if mode == "off" {
		log.Printf("sandbox: выключена (KRISTINA_SANDBOX=off)")
		return nil
	}
	fail := func(format string, args ...any) sandbox {
		if mode == "docker" {
			log.Fatalf("sandbox: "+format, args...)
		}
		log.Printf("sandbox: нет — "+format+"; файлы при этом работают", args...)
		return nil
	}
	bin, err := exec.LookPath(envStr("KRISTINA_DOCKER_BIN", "docker"))
	if err != nil {
		return fail("docker CLI не найден")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, bin, "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		return fail("докер не отвечает: %s", strings.TrimSpace(string(out)))
	}
	image := envStr("KRISTINA_SANDBOX_IMAGE", "kristina-sandbox:local")
	if err := exec.CommandContext(ctx, bin, "image", "inspect", image).Run(); err != nil {
		return fail("образа %s нет (docker compose --profile sandbox build sandbox)", image)
	}
	// Докер монтирует путь хоста. Бот в контейнере видит workspace по
	// другому пути — тогда хостовый задаётся явно (docker-compose.yml)
	hostDir := os.Getenv("KRISTINA_SANDBOX_HOST_DIR")
	if hostDir == "" {
		if hostDir, err = filepath.Abs(wsDir); err != nil {
			return fail("%v", err)
		}
	}
	log.Printf("sandbox: %s, сеть %s, workspace %s", image, network, hostDir)
	return &dockerSandbox{bin: bin, image: image, hostDir: hostDir, network: network,
		memory: envStr("KRISTINA_SANDBOX_MEMORY", "1g"), cpus: envStr("KRISTINA_SANDBOX_CPUS", "1")}
}

func envDuration(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Fatalf("%s: %v", k, err)
	}
	return d
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
