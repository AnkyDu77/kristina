package main

import (
	"testing"
	"time"
)

func TestGPUAcquire(t *testing.T) {
	// Без newGPUManager — watchdog в тесте не нужен
	m := &gpuManager{state: "absent", idleTimeout: time.Minute}
	if _, _, ok := m.acquire(); ok {
		t.Fatal("спящую машину занимать нельзя")
	}

	m.state, m.endpoint = "running", "http://gpu:8080"
	ep, release, ok := m.acquire()
	if !ok || ep != "http://gpu:8080" {
		t.Fatalf("acquire: ok=%v ep=%q", ok, ep)
	}
	if st := m.status(); st.IdleLeft != time.Minute {
		t.Fatalf("пока запрос в полёте, отсчёт простоя стоит: %s", st.IdleLeft)
	}
	release()
	release() // повторный release не должен уводить счётчик в минус
	if m.inFlight != 0 {
		t.Fatalf("inFlight = %d", m.inFlight)
	}
}
