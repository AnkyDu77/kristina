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
)

type sandbox interface {
	run(ctx context.Context, command string, timeout time.Duration) (out string, code int, err error)
	status(ctx context.Context) string
	reset(ctx context.Context) error
}

type dockerSandbox struct {
	bin, image, hostDir, network, memory, cpus string

	mu sync.Mutex // подъём контейнера — один за раз
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
	out, err := s.docker(ctx, "run", "-d", "--name", sandboxName, "--init",
		"--user", "1000:1000", "--network", s.network,
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--read-only", "--tmpfs", "/tmp:rw,exec,size=512m",
		"--memory", s.memory, "--memory-swap", s.memory, "--cpus", s.cpus, "--pids-limit", "256",
		"-e", "HOME=/tmp", "-e", "GOCACHE=/tmp/go-cache", "-e", "GOPATH=/tmp/go",
		"-v", s.hostDir+":/workspace", "-w", "/workspace",
		"--label", "kristina=sandbox",
		s.image, "sleep", "infinity")
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
	t := &tool{
		spec: toolSpecFunc{
			Name: "run_command",
			Description: "Выполнить shell-команду (sh) в своей Linux-песочнице: рабочая папка /workspace — там файлы владельца и твои. " +
				"Есть python3, go, git, jq, ripgrep. " + b.sandboxNetNote() +
				" Потолок — " + strconv.Itoa(int(commandTimeout.Seconds())) + " секунд на команду. Возвращает вывод и код выхода.",
			Parameters: object(map[string]any{"command": str("команда для sh -c")}, "command"),
		},
		modes:  everywhere,
		status: func(a json.RawMessage) string { return "💻 " + truncRunes(argStr(a, "command"), 100) },
		run: func(ctx context.Context, _ int64, a json.RawMessage) (string, error) {
			command := strings.TrimSpace(argStr(a, "command"))
			if command == "" {
				return "", errors.New("пустая команда")
			}
			out, code, err := b.sandbox.run(ctx, command, commandTimeout)
			if err != nil {
				return "", err
			}
			return formatRun(command, out, code), nil
		},
	}
	// С сетью песочница может ходить куда угодно — тогда каждая команда
	// с ✅ владельца
	if b.cfg.sandboxNetwork != "none" {
		t.confirm = func(_ int64, a json.RawMessage) (string, error) {
			return "Выполнить в песочнице (с сетью): " + truncRunes(argStr(a, "command"), 400), nil
		}
	}
	return []*tool{t}
}

func (b *bot) sandboxNetNote() string {
	if b.cfg.sandboxNetwork == "none" {
		return "Сети нет: ничего не скачать (pip/go get не сработают) — за данными иди web_search/read_page/браузером."
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
