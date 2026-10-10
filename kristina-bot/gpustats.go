package main

// Состояние видеокарты в чате: снимок nvidia-smi, который GPU-машина
// раз в 5 секунд кладёт в /var/lib/kristina-gpu, а nginx отдаёт по
// <медиа-API>/gpu/ (selectel/main.tf).
//
// Статистику спрашиваем, НЕ занимая машину (без acquire): иначе каждый
// /gpu сдвигал бы отсчёт простоя, и карта, на которую просто смотрят,
// никогда бы не погасла. По той же причине спящую карту ради статистики
// не будим.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type gpuInfo struct {
	Index, Name, PState      string
	Util, MemUtil            float64 // %
	MemUsed, MemTotal        float64 // МиБ
	Temp, Power, PowerLimit  float64 // °C, Вт
	ClockSM                  float64 // МГц
	hasUtil, hasTemp, hasPow bool
}

type gpuApp struct {
	PID, Name string
	Mem       float64 // МиБ
}

var errNoGPUStats = errors.New("статистики нет: машина поднята до этого обновления — появится после следующего подъёма")

// gpuBase — адрес работающей машины, не занимая её.
func (b *bot) gpuBase() (string, error) {
	if b.gpu == nil {
		if b.cfg.staticBase == "" {
			return "", errGPUAsleep
		}
		return b.cfg.staticBase, nil
	}
	st := b.gpu.status()
	if st.State != "running" {
		return "", errGPUAsleep
	}
	return st.Endpoint, nil
}

func (b *bot) fetchGPUFile(ctx context.Context, base, name string) (string, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/gpu/"+name, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+b.media.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", time.Time{}, errNoGPUStats
	case resp.StatusCode != http.StatusOK:
		return "", time.Time{}, fmt.Errorf("статистика GPU: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	taken, _ := http.ParseTime(resp.Header.Get("Last-Modified"))
	return string(data), taken, err
}

func parseGPUs(csv string) []gpuInfo {
	var out []gpuInfo
	for _, line := range strings.Split(strings.TrimSpace(csv), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 11 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		num := func(s string) (float64, bool) {
			v, err := strconv.ParseFloat(s, 64)
			return v, err == nil // «[N/A]» у датацентровых карт — бывает
		}
		g := gpuInfo{Index: f[0], Name: f[1], PState: f[2]}
		g.Util, g.hasUtil = num(f[3])
		g.MemUtil, _ = num(f[4])
		g.MemUsed, _ = num(f[5])
		g.MemTotal, _ = num(f[6])
		g.Temp, g.hasTemp = num(f[7])
		g.Power, g.hasPow = num(f[8])
		g.PowerLimit, _ = num(f[9])
		g.ClockSM, _ = num(f[10])
		out = append(out, g)
	}
	return out
}

func parseApps(csv string) []gpuApp {
	var out []gpuApp
	for _, line := range strings.Split(strings.TrimSpace(csv), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 3 {
			continue
		}
		mem, _ := strconv.ParseFloat(strings.TrimSpace(f[2]), 64)
		out = append(out, gpuApp{PID: strings.TrimSpace(f[0]), Name: strings.TrimSpace(f[1]), Mem: mem})
	}
	return out
}

// appLabel — кто это на карте, по-человечески.
func appLabel(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "lipsync"):
		return "губы (MuseTalk)"
	case strings.Contains(n, "tts"):
		return "голос (Qwen3-TTS)"
	case strings.Contains(n, "llmster"), strings.Contains(n, "lmstudio"), strings.Contains(n, "lm-studio"), strings.Contains(n, "llama"):
		return "мозг (LM Studio)"
	}
	return name
}

func bar(pct float64) string {
	const n = 10
	full := int(pct/100*n + 0.5)
	full = max(0, min(n, full))
	return strings.Repeat("▓", full) + strings.Repeat("░", n-full)
}

func gib(mib float64) string { return fmt.Sprintf("%.1f", mib/1024) }

func formatGPUStats(gpus []gpuInfo, apps []gpuApp, taken time.Time) string {
	var sb strings.Builder
	for _, g := range gpus {
		fmt.Fprintf(&sb, "🖥 %s", g.Name)
		if len(gpus) > 1 {
			fmt.Fprintf(&sb, " #%s", g.Index)
		}
		if g.PState != "" {
			fmt.Fprintf(&sb, " · %s", g.PState)
		}
		sb.WriteString("\n")
		if g.hasUtil {
			fmt.Fprintf(&sb, "Загрузка  %s %3.0f%%", bar(g.Util), g.Util)
			if g.ClockSM > 0 {
				fmt.Fprintf(&sb, " · %.0f МГц", g.ClockSM)
			}
			sb.WriteString("\n")
		}
		if g.MemTotal > 0 {
			pct := g.MemUsed / g.MemTotal * 100
			fmt.Fprintf(&sb, "VRAM      %s %3.0f%% · %s / %s ГБ, свободно %s\n",
				bar(pct), pct, gib(g.MemUsed), gib(g.MemTotal), gib(g.MemTotal-g.MemUsed))
		}
		var tail []string
		if g.hasTemp {
			tail = append(tail, fmt.Sprintf("%.0f°C", g.Temp))
		}
		if g.hasPow {
			p := fmt.Sprintf("%.0f Вт", g.Power)
			if g.PowerLimit > 0 {
				p = fmt.Sprintf("%.0f / %.0f Вт", g.Power, g.PowerLimit)
			}
			tail = append(tail, p)
		}
		if len(tail) > 0 {
			sb.WriteString(strings.Join(tail, " · ") + "\n")
		}
	}
	if len(apps) > 0 {
		sb.WriteString("\nНа карте:\n")
		for _, a := range apps {
			fmt.Fprintf(&sb, "• %s — %s ГБ\n", appLabel(a.Name), gib(a.Mem))
		}
	}
	if !taken.IsZero() {
		fmt.Fprintf(&sb, "\nСнимок %d с назад", int(max(0, time.Since(taken).Seconds())))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// gpuReport — сводка (или сырой nvidia-smi при full) и строка от бота:
// сколько машина работает и когда погаснет.
func (b *bot) gpuReport(ctx context.Context, full bool) (string, error) {
	base, err := b.gpuBase()
	if err != nil {
		return "", err
	}
	var text string
	if full {
		raw, _, err := b.fetchGPUFile(ctx, base, "smi.txt")
		if err != nil {
			return "", err
		}
		text = "```\n" + strings.TrimRight(raw, "\n") + "\n```"
	} else {
		gpuCSV, taken, err := b.fetchGPUFile(ctx, base, "gpu.csv")
		if err != nil {
			return "", err
		}
		appsCSV, _, err := b.fetchGPUFile(ctx, base, "apps.csv")
		if err != nil && !errors.Is(err, errNoGPUStats) {
			return "", err
		}
		gpus := parseGPUs(gpuCSV)
		if len(gpus) == 0 {
			return "", fmt.Errorf("nvidia-smi ответил странно: %s", truncRunes(strings.TrimSpace(gpuCSV), 300))
		}
		text = formatGPUStats(gpus, parseApps(appsCSV), taken)
	}
	if b.gpu != nil {
		st := b.gpu.status()
		text += fmt.Sprintf("\nМашина работает %s, авто-стоп через %s", st.Up.Round(time.Minute), st.IdleLeft.Round(time.Minute))
	}
	return text, nil
}

func (b *bot) gpuCmd(ctx context.Context, chatID int64, arg string) error {
	text, err := b.gpuReport(ctx, arg == "full")
	switch {
	case errors.Is(err, errGPUAsleep):
		msg := "Видеокарта спит — смотреть не на что, а будить ради статистики не стану."
		if b.gpu != nil && b.gpu.status().State == "provisioning" {
			msg = "Видеокарта как раз просыпается — спроси через пару минут."
		}
		return b.tg.sendPlain(ctx, chatID, msg)
	case errors.Is(err, errNoGPUStats):
		return b.tg.sendPlain(ctx, chatID, err.Error())
	case err != nil:
		return err
	}
	if arg == "full" {
		return b.sendMarkdown(ctx, chatID, text)
	}
	// Полоски ровняются только моноширинным шрифтом
	return b.sendMarkdown(ctx, chatID, "```\n"+text+"\n```\nПолный вывод nvidia-smi: /gpu full")
}

func (b *bot) gpuTools() []*tool {
	return []*tool{{
		spec: toolSpecFunc{
			Name:        "gpu_status",
			Description: "Состояние своей видеокарты (как nvidia-smi): загрузка, VRAM, температура, питание, кто сколько памяти занял, когда машина погаснет.",
			Parameters:  object(map[string]any{}),
		},
		modes:     everywhere,
		available: func() bool { _, err := b.gpuBase(); return err == nil },
		status:    func(json.RawMessage) string { return "🖥 Смотрю на видеокарту" },
		run: func(ctx context.Context, _ int64, _ json.RawMessage) (string, error) {
			return b.gpuReport(ctx, false)
		},
	}}
}
