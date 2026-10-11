package main

// Компьютер Кристины — контейнер-песочница на VPS бота: выполнить
// команду, собрать и запустить код, разобрать файл. Папка /workspace
// внутри — это data/workspace бота: файлы, которые ты присылаешь в
// телеграм, лежат там же, и песочница их видит.
//
// Изоляция: пользователь 1000 без capabilities, корень только на чтение,
// --network none (по умолчанию), лимиты памяти, CPU и числа процессов, на
// команду — потолок времени. Секретов бота внутри нет: смонтирована только
// workspace. Бот управляет песочницей через docker CLI и сокет докера —
// сама модель до сокета не дотягивается, у неё есть только «выполни это
// в песочнице».

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	sandboxName      = "kristina-sandbox"
	sandboxMaxOutput = 256 << 10 // больше команде не дадим вывести — защита памяти бота
	commandTimeout   = 60 * time.Second
	installTimeout   = 10 * time.Minute
)

type sandbox interface {
	run(ctx context.Context, command string, timeout time.Duration) (out string, code int, err error)
	// install — установка зависимостей в одноразовом контейнере С сетью:
	// argv без шелла, рабочая папка — dir внутри /workspace
	install(ctx context.Context, dir string, argv []string, timeout time.Duration) (out string, code int, err error)
	status(ctx context.Context) string
	reset(ctx context.Context) error
}

type dockerSandbox struct {
	bin, image, hostDir, network, memory, swap, cpus string
	// installNet — сеть для install_deps; пусто — установка выключена
	installNet string

	mu sync.Mutex // подъём контейнера — один за раз
}

// isolation — общее для песочницы и установщика: кто, с какими правами и
// лимитами, что смонтировано. Кэши сборки — в /workspace/.cache: зависимости,
// поставленные install_deps (с сетью), видны run_command (без сети), а
// холодная сборка Go на одном ядре не повторяется после каждого reset.
func (s *dockerSandbox) isolation() []string {
	swap := s.swap
	if swap == "" {
		swap = s.memory
	}
	return []string{"--init", "--user", "1000:1000",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--read-only", "--tmpfs", "/tmp:rw,exec,size=512m",
		"--memory", s.memory, "--memory-swap", swap, "--cpus", s.cpus, "--pids-limit", "512",
		"-e", "HOME=/tmp", "-e", "GOPATH=/tmp/go",
		"-e", "GOCACHE=/workspace/.cache/go-build", "-e", "GOMODCACHE=/workspace/.cache/go-mod",
		"-e", "GOLANGCI_LINT_CACHE=/workspace/.cache/golangci-lint", "-e", "npm_config_cache=/workspace/.cache/npm",
		"-e", "npm_config_update_notifier=false", "-e", "npm_config_fund=false",
		"-v", s.hostDir + ":/workspace",
		"--label", "kristina=sandbox"}
}

func (s *dockerSandbox) docker(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, s.bin, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// ensure поднимает песочницу, если её нет: команды идут в долгоживущий
// контейнер, а не в новый на каждую — так между ними живут процессы и /tmp.
func (s *dockerSandbox) ensure(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, err := s.docker(ctx, "inspect", "-f", "{{.State.Running}}", sandboxName); err == nil && st == "true" {
		return nil
	}
	_, _ = s.docker(ctx, "rm", "-f", sandboxName)
	args := append([]string{"run", "-d", "--name", sandboxName, "--network", s.network, "-w", "/workspace"}, s.isolation()...)
	out, err := s.docker(ctx, append(args, s.image, "sleep", "infinity")...)
	if err != nil {
		return fmt.Errorf("песочница не поднялась: %s", truncRunes(out, 400))
	}
	return nil
}

func (s *dockerSandbox) run(ctx context.Context, command string, timeout time.Duration) (string, int, error) {
	if err := s.ensure(ctx); err != nil {
		return "", -1, err
	}
	secs := strconv.Itoa(int(timeout.Seconds()))
	// Потолок времени — внутри контейнера: убитый docker exec процесс в
	// песочнице не убивает
	cctx, cancel := context.WithTimeout(ctx, timeout+15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, s.bin, "exec", "-w", "/workspace", sandboxName,
		"timeout", "-s", "KILL", secs, "sh", "-c", command)
	var out cappedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return out.String(), 0, nil
	case errors.As(err, &ee):
		return out.String(), ee.ExitCode(), nil
	}
	return out.String(), -1, err
}

func (s *dockerSandbox) install(ctx context.Context, dir string, argv []string, timeout time.Duration) (string, int, error) {
	if s.installNet == "" {
		return "", -1, errors.New("установка зависимостей выключена (KRISTINA_SANDBOX_INSTALL_NETWORK)")
	}
	secs := strconv.Itoa(int(timeout.Seconds()))
	cctx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()
	args := append([]string{"run", "--rm", "--network", s.installNet, "-w", "/workspace/" + dir}, s.isolation()...)
	args = append(args, s.image, "timeout", "-s", "KILL", secs)
	cmd := exec.CommandContext(cctx, s.bin, append(args, argv...)...)
	var out cappedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return out.String(), 0, nil
	case errors.As(err, &ee):
		return out.String(), ee.ExitCode(), nil
	}
	return out.String(), -1, err
}

func (s *dockerSandbox) status(ctx context.Context) string {
	st, err := s.docker(ctx, "inspect", "-f", "{{.State.Status}} · {{.Config.Image}}", sandboxName)
	if err != nil {
		return "не запущена (поднимется на первой команде), образ " + s.image
	}
	return st
}

func (s *dockerSandbox) reset(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if out, err := s.docker(ctx, "rm", "-f", sandboxName); err != nil && !strings.Contains(out, "No such container") {
		return fmt.Errorf("%s", out)
	}
	return nil
}

// cappedBuffer — вывод команды, но не больше sandboxMaxOutput: `yes` на
// минуту не должен съесть память бота.
type cappedBuffer struct {
	buf     []byte
	dropped int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := sandboxMaxOutput - len(c.buf); room > 0 {
		c.buf = append(c.buf, p[:min(room, len(p))]...)
		c.dropped += len(p) - min(room, len(p))
	} else {
		c.dropped += len(p)
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	s := strings.ToValidUTF8(string(c.buf), "�")
	if c.dropped > 0 {
		s += fmt.Sprintf("\n…[ещё %d байт вывода отброшено]", c.dropped)
	}
	return s
}

// formatRun — что увидит модель. Хвост вывода важнее головы: ошибка
// компилятора и итог тестов — в конце.
func formatRun(command, out string, code int) string {
	out = strings.TrimRight(out, "\n")
	if n := utf8.RuneCountInString(out); n > maxToolResult-200 {
		r := []rune(out)
		out = "…(начало обрезано)\n" + string(r[n-(maxToolResult-200):])
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "$ %s\n", truncRunes(command, 300))
	if out != "" {
		sb.WriteString(out + "\n")
	}
	switch code {
	case 0:
		sb.WriteString("[готово, код 0]")
	case 137:
		sb.WriteString("[убито: вышло время или память]")
	default:
		fmt.Fprintf(&sb, "[код выхода %d]", code)
	}
	return sb.String()
}

func (b *bot) computerTools() []*tool {
	if b.sandbox == nil {
		return nil
	}
	maxTimeout := b.cfg.sandboxMaxTimeout
	if maxTimeout <= 0 {
		maxTimeout = installTimeout
	}
	run := &tool{
		spec: toolSpecFunc{
			Name: "run_command",
			Description: "Выполнить shell-команду (sh) в своей Linux-песочнице; рабочая папка /workspace — там файлы владельца и твои. " +
				"Есть: go 1.27 (+gcc для cgo, staticcheck, goimports, golangci-lint), node 22 + npm, python3, git, make, jq, ripgrep. " +
				b.sandboxNetNote() + " Зависимости ставь через install_deps, а собирай и проверяй здесь. " +
				fmt.Sprintf("Потолок по умолчанию — %d с; для сборки и тестов передай timeout_sec (до %d).",
					int(commandTimeout.Seconds()), int(maxTimeout.Seconds())),
			Parameters: object(map[string]any{
				"command":     str("команда для sh -c"),
				"timeout_sec": map[string]any{"type": "integer", "description": "потолок времени, секунд"},
			}, "command"),
		},
		modes:   everywhere,
		timeout: maxTimeout + 45*time.Second,
		status:  func(a json.RawMessage) string { return "💻 " + truncRunes(argStr(a, "command"), 100) },
		run: func(ctx context.Context, _ int64, a json.RawMessage) (string, error) {
			command := strings.TrimSpace(argStr(a, "command"))
			if command == "" {
				return "", errors.New("пустая команда")
			}
			out, code, err := b.sandbox.run(ctx, command, clampTimeout(argInt(a, "timeout_sec"), commandTimeout, maxTimeout))
			if err != nil {
				return "", err
			}
			return formatRun(command, out, code), nil
		},
	}
	// С сетью песочница может ходить куда угодно — тогда каждая команда
	// с ✅ владельца
	if b.cfg.sandboxNetwork != "none" {
		run.confirm = func(_ int64, a json.RawMessage) (string, error) {
			return "Выполнить в песочнице (с сетью): " + truncRunes(argStr(a, "command"), 400), nil
		}
	}
	tools := []*tool{run}

	if b.cfg.sandboxInstall && b.ws != nil {
		tools = append(tools, &tool{
			spec: toolSpecFunc{
				Name: "install_deps",
				Description: "Поставить зависимости проекта — единственное место, где у песочницы есть интернет: " +
					"npm ci | npm install [пакеты] | go mod download | go mod tidy | go get <модуль>. " +
					"dir — папка проекта в /workspace. Каждый вызов подтверждает владелец. " +
					"Дальше собирай и проверяй через run_command: node_modules и кэш модулей Go останутся.",
				Parameters: object(map[string]any{
					"dir":     str("папка проекта внутри /workspace, например academy-remont"),
					"command": str("например «npm ci» или «go mod download»"),
				}, "dir", "command"),
			},
			modes:   everywhere,
			timeout: installTimeout + 45*time.Second,
			status:  func(a json.RawMessage) string { return "📦 " + argStr(a, "dir") + ": " + argStr(a, "command") },
			confirm: func(_ int64, a json.RawMessage) (string, error) {
				dir, argv, err := b.installArgs(a)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("Поставить зависимости с доступом в интернет: /workspace/%s → %s", dir, strings.Join(argv, " ")), nil
			},
			run: func(ctx context.Context, _ int64, a json.RawMessage) (string, error) {
				dir, argv, err := b.installArgs(a)
				if err != nil {
					return "", err
				}
				out, code, err := b.sandbox.install(ctx, dir, argv, installTimeout)
				if err != nil {
					return "", err
				}
				return formatRun(strings.Join(argv, " "), out, code), nil
			},
		})
	}
	return tools
}

// installCommands — что можно запустить с сетью. Только менеджеры пакетов
// и только эти подкоманды: «npm run build» с интернетом — уже не установка.
// Шелла нет (argv как есть), так что «npm ci; curl …» не пройдёт.
var installCommands = [][]string{
	{"npm", "ci"}, {"npm", "install"}, {"npm", "i"},
	{"go", "mod", "download"}, {"go", "mod", "tidy"}, {"go", "get"},
}

var installArgRe = regexp.MustCompile(`^[A-Za-z0-9@._/=:^~<>+,-]+$`)

func (b *bot) installArgs(a json.RawMessage) (dir string, argv []string, err error) {
	dir, err = b.ws.rel(argStr(a, "dir"))
	if err != nil {
		return "", nil, err
	}
	argv = strings.Fields(argStr(a, "command"))
	allowed := false
	for _, prefix := range installCommands {
		if len(argv) >= len(prefix) && strings.Join(argv[:len(prefix)], " ") == strings.Join(prefix, " ") {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", nil, errors.New("install_deps умеет только npm ci / npm install / go mod download / go mod tidy / go get; остальное — run_command")
	}
	for _, arg := range argv {
		if !installArgRe.MatchString(arg) {
			return "", nil, fmt.Errorf("подозрительный аргумент %q", arg)
		}
	}
	return dir, argv, nil
}

func clampTimeout(sec int64, def, max time.Duration) time.Duration {
	if sec <= 0 {
		return def
	}
	return min(time.Duration(sec)*time.Second, max)
}

func (b *bot) sandboxNetNote() string {
	if b.cfg.sandboxNetwork == "none" {
		return "Сети нет: скачать ничего нельзя — за данными иди web_search/read_page/браузером."
	}
	return "Сеть есть, каждую команду подтверждает владелец."
}

func (b *bot) computerCmd(ctx context.Context, chatID int64, arg string) error {
	if b.sandbox == nil {
		return b.tg.sendPlain(ctx, chatID, "Песочницы нет: докер недоступен боту (см. KRISTINA_SANDBOX в README). Файлы при этом работают: /files.")
	}
	if arg == "reset" {
		if err := b.sandbox.reset(ctx); err != nil {
			return err
		}
		return b.tg.sendPlain(ctx, chatID, "Песочницу снесла — поднимется заново на первой команде. Файлы в workspace на месте.")
	}
	return b.tg.sendPlain(ctx, chatID, "Песочница: "+b.sandbox.status(ctx)+"\nСеть: "+b.cfg.sandboxNetwork+
		"\n\n/computer reset — пересоздать (файлы останутся), /files — что в workspace.")
}
