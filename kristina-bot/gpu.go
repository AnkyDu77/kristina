package main

// gpuManager — жизненный цикл GPU VPS через terraform, урезанная копия
// lmify-ui/gpu.go: одна машина (Кристина личная), без HTTP-панели.
// Машину будит только владелец (/wake или кнопкой), гасит — watchdog
// после простоя: карта тикает по счёту, только пока Кристина думает.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	// Сколько раз пробуем снести машину, прежде чем сдаться: обычный
	// сетевой сбой облачного API на повторе проходит.
	destroyAttempts   = 3
	destroyRetryDelay = 20 * time.Second
	logTailLines      = 40
)

type gpuManager struct {
	tfBin, tfDir string
	idleTimeout  time.Duration

	mu           sync.Mutex
	state        string // absent | provisioning | running | destroying | error
	endpoint     string
	since        time.Time
	lastActivity time.Time
	inFlight     int // запросы в полёте — watchdog их не прерывает
	errMsg       string
	tail         []string // последние строки terraform — для отчёта об ошибке
	// onReady — машина поднялась: бот отвечает на отложенные вопросы
	onReady func()
	// notify — сказать владельцу по ходу подъёма (запасной путь и т.п.)
	notify func(string)

	// Запасной путь: прерываемой карты нет → повторить apply обычным
	// сервером. fault — почему сервер ушёл в ERROR (из API Selectel)
	ondemandFallback bool
	fault            func(id string) (string, error)
	onDemand         bool // поднят обычный (непрерываемый) сервер — дороже
}

// instanceAddr — ресурс сервера в terraform: запасной путь пересоздаёт
// только его (-replace), а сеть, IP и диск остаются как есть.
const instanceAddr = "openstack_compute_instance_v2.kristina_vps"

func (m *gpuManager) say(msg string) {
	m.mu.Lock()
	f := m.notify
	m.mu.Unlock()
	if f != nil {
		f(msg)
	}
}

func (m *gpuManager) setNotify(f func(string)) {
	m.mu.Lock()
	m.notify = f
	m.mu.Unlock()
}

func (m *gpuManager) setOnReady(f func()) {
	m.mu.Lock()
	m.onReady = f
	m.mu.Unlock()
}

func newGPUManager(tfBin, tfDir string, idle time.Duration) *gpuManager {
	m := &gpuManager{tfBin: tfBin, tfDir: tfDir, idleTimeout: idle, state: "absent"}
	go m.watchdog()
	return m
}

// syncFromState подхватывает VPS, пережившую рестарт бота. Не подхватить
// её значит бросить крутиться без watchdog-а.
func (m *gpuManager) syncFromState() bool {
	outs, err := m.readOutputs()
	if err != nil || outs["media_endpoint"] == "" {
		return false
	}
	m.mu.Lock()
	m.state, m.endpoint = "running", outs["media_endpoint"]
	m.onDemand = outs["preemptible"] == "false"
	m.since, m.lastActivity = time.Now(), time.Now()
	m.mu.Unlock()
	log.Printf("gpu: подхвачена работающая VPS из terraform state: %s", outs["media_endpoint"])
	return true
}

type gpuStatus struct {
	State    string
	OnDemand bool // обычный сервер вместо прерываемого
	Endpoint string
	Up       time.Duration
	IdleLeft time.Duration
	Err      string
}

func (m *gpuManager) status() gpuStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := gpuStatus{State: m.state, Endpoint: m.endpoint, Err: m.errMsg, OnDemand: m.onDemand}
	if m.state == "running" {
		st.Up = time.Since(m.since)
		st.IdleLeft = max(0, m.idleTimeout-time.Since(m.lastActivity))
		if m.inFlight > 0 {
			st.IdleLeft = m.idleTimeout
		}
	}
	return st
}

// acquire занимает работающую машину под запрос. Проверка состояния и
// инкремент — под одним замком: иначе watchdog мог бы снести машину
// между «она работает» и «я ей пользуюсь».
func (m *gpuManager) acquire() (endpoint string, release func(), ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != "running" {
		return "", nil, false
	}
	m.inFlight++
	m.lastActivity = time.Now()
	var once sync.Once
	return m.endpoint, func() {
		once.Do(func() {
			m.mu.Lock()
			m.inFlight--
			m.lastActivity = time.Now()
			m.mu.Unlock()
		})
	}, true
}

// waitRunning ждёт, пока машина поднимется. Ошибка — если apply упал
// или машину погасили, пока ждали.
func (m *gpuManager) waitRunning(ctx context.Context) error {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		st := m.status()
		switch st.State {
		case "running":
			return nil
		case "error":
			return fmt.Errorf("GPU VPS не поднялась: %s", st.Err)
		case "absent", "destroying":
			return fmt.Errorf("GPU VPS погашена, пока мы её ждали")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (m *gpuManager) watchdog() {
	for range time.Tick(30 * time.Second) {
		m.mu.Lock()
		// «error» сторожим наравне с «running»: упавший apply/destroy
		// оставляет машину (или её половину) живой и платной.
		alive := m.state == "running" || m.state == "error"
		idle := alive && m.inFlight == 0 && time.Since(m.lastActivity) > m.idleTimeout
		m.mu.Unlock()
		if !idle {
			continue
		}
		log.Printf("gpu: простой > %s — авто-отключение", m.idleTimeout)
		if err := m.stop(); err != nil {
			log.Printf("gpu: watchdog destroy: %v", err)
		}
	}
}

func (m *gpuManager) start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch m.state {
	case "provisioning", "destroying":
		return fmt.Errorf("операция уже выполняется")
	case "running":
		return fmt.Errorf("GPU VPS уже работает")
	}
	m.beginOpLocked("provisioning")
	go m.runOp("apply")
	return nil
}

func (m *gpuManager) stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == "provisioning" || m.state == "destroying" {
		return fmt.Errorf("операция уже выполняется")
	}
	if m.state != "running" && m.state != "error" {
		return fmt.Errorf("GPU VPS не запущена")
	}
	m.beginOpLocked("destroying")
	go m.runOp("destroy")
	return nil
}

func (m *gpuManager) beginOpLocked(state string) {
	m.state, m.errMsg, m.endpoint = state, "", ""
	m.tail = m.tail[:0]
}

func (m *gpuManager) appendLog(line string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.tail) >= logTailLines {
		m.tail = m.tail[1:]
	}
	m.tail = append(m.tail, line)
}

func (m *gpuManager) logTail() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.tail, "\n")
}

func (m *gpuManager) tfCmd(args ...string) *exec.Cmd {
	cmd := exec.Command(m.tfBin, args...)
	cmd.Dir = m.tfDir
	cmd.Env = os.Environ()
	return cmd
}

// runTF выполняет terraform, отдавая вывод и в лог процесса (apply идёт
// десятки минут — пусть будет видно, на чём он), и в хвост для отчёта.
// Возвращает и весь вывод: по нему решаем, не нехватка ли это карт.
func (m *gpuManager) runTF(args []string) (string, error) {
	cmd := m.tfCmd(args...)
	var out strings.Builder
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw

	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 256*1024), 256*1024)
		for sc.Scan() {
			log.Printf("tf: %s", sc.Text())
			m.appendLog(sc.Text())
			if out.Len() < 4<<20 {
				out.WriteString(sc.Text() + "\n")
			}
		}
	}()

	err := cmd.Run()
	pw.Close()
	<-done
	return out.String(), err
}

func (m *gpuManager) runOp(verb string) {
	args := []string{verb, "-auto-approve", "-no-color", "-input=false"}

	// destroy идемпотентен и повторяется; apply — нет: второй заход
	// поверх наполовину созданного стоит дороже разбора вручную.
	attempts := 1
	if verb == "destroy" {
		attempts = destroyAttempts
	}
	var err error
	var out string
	onDemand := false
	for i := 1; i <= attempts; i++ {
		if out, err = m.runTF(args); err == nil {
			break
		}
		if i < attempts {
			log.Printf("gpu: destroy, попытка %d из %d: %v — повтор через %s", i, attempts, err, destroyRetryDelay)
			time.Sleep(destroyRetryDelay)
		}
	}

	// Двухстадийный подъём: прерываемой карты нет — пересоздаём только
	// сервер, обычным. Остальное (сеть, IP, диск) уже создано первым заходом
	// и остаётся. Причину берём у самого Selectel: терраформ её не пишет
	if err != nil && verb == "apply" {
		capacity, why := capacityVerdict(out, m.fault)
		switch {
		case capacity && m.ondemandFallback:
			log.Printf("gpu: прерываемой карты нет (%s) — поднимаю обычный сервер", why)
			m.say(fmt.Sprintf("Прерываемой карты у Selectel сейчас нет (%s). Поднимаю обычную — она дороже, зато будет; "+
				"гаснет по простою так же.", why))
			out, err = m.runTF(append(args, "-var", "preemptible=false", "-replace="+instanceAddr))
			onDemand = err == nil
			if err != nil {
				if again, why2 := capacityVerdict(out, m.fault); again {
					err = fmt.Errorf("у Selectel нет свободных карт этого типа — ни прерываемых, ни обычных (%s)", why2)
				}
			}
		case why != "":
			err = fmt.Errorf("%w; причина: %s", err, why)
		}
	}

	var ep string
	if err == nil && verb == "apply" {
		outs, oerr := m.readOutputs()
		if ep = outs["media_endpoint"]; oerr != nil || ep == "" {
			err = fmt.Errorf("apply прошёл, но outputs не прочитались")
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.state = "error"
		m.errMsg = fmt.Sprintf("terraform %s: %v", verb, err)
		// Отсчёт до авто-зачистки — с момента сбоя
		m.lastActivity = time.Now()
		log.Printf("gpu: %s", m.errMsg)
		return
	}
	if verb == "apply" {
		m.state, m.endpoint, m.onDemand = "running", ep, onDemand
		m.since, m.lastActivity = time.Now(), time.Now()
		log.Printf("gpu: VPS готова: %s", ep)
		if m.onReady != nil {
			go m.onReady()
		}
		return
	}
	m.state = "absent"
	log.Printf("gpu: VPS уничтожена")
}

func (m *gpuManager) readOutputs() (map[string]string, error) {
	raw, err := m.tfCmd("output", "-json").Output()
	if err != nil {
		return nil, err
	}
	var parsed map[string]struct {
		Value any `json:"value"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	outs := make(map[string]string, len(parsed))
	for k, v := range parsed {
		if s, ok := v.Value.(string); ok {
			outs[k] = s
		}
	}
	return outs, nil
}
