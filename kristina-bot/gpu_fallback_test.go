package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Как провайдер openstack 3.4.0 сообщает об упавшем сервере — без причины
const errorInstance = "Error: Error waiting for instance (0f6d3b8e-1c2a-4e5f-9a7b-3c4d5e6f7a8b) to become ready: " +
	"unexpected state 'ERROR', wanted target 'ACTIVE'. last error: %!s(<nil>)"

func TestCapacityVerdict(t *testing.T) {
	faultIs := func(msg string, err error) func(string) (string, error) {
		return func(id string) (string, error) {
			if id != "0f6d3b8e-1c2a-4e5f-9a7b-3c4d5e6f7a8b" {
				t.Errorf("id сервера: %q", id)
			}
			return msg, err
		}
	}
	for name, c := range map[string]struct {
		out      string
		fault    func(string) (string, error)
		capacity bool
		reason   string
	}{
		"нет хостов (из API)": {errorInstance, faultIs("No valid host was found. There are not enough hosts available.", nil), true, "No valid host"},
		"нет хостов (в логе)": {"Error: No valid host was found", nil, true, "No valid host"},
		"по-русски":           {errorInstance, faultIs("Недостаточно ресурсов в регионе", nil), true, "Недостаточно ресурсов"},
		"квота в логе":        {"Error: Quota exceeded for instances", nil, false, "квоту"},
		"квота в API":         {errorInstance, faultIs("Quota exceeded for cores: Requested 16", nil), false, "Quota"},
		"другая причина":      {errorInstance, faultIs("Image 1234 could not be found.", nil), false, "Image 1234"},
		"API недоступен":      {errorInstance, faultIs("", errors.New("keystone: 401")), true, "не сообщил"},
		"упало не на сервере": {"Error: creating network: 409 Conflict", nil, false, ""},
	} {
		capacity, reason := capacityVerdict(c.out, c.fault)
		if capacity != c.capacity || !strings.Contains(reason, c.reason) {
			t.Errorf("%s: capacity=%v reason=%q", name, capacity, reason)
		}
	}
}

// fakeTF — terraform-скрипт: первый apply падает как задано, apply с
// preemptible=false — как задано вторым. Вызовы пишет в calls.
func fakeTF(t *testing.T, first, second string, secondOK bool) (bin, dir string, calls func() []string) {
	t.Helper()
	dir = t.TempDir()
	code := 1
	if secondOK {
		code = 0
	}
	script := fmt.Sprintf(`#!/bin/bash
D="$(dirname "$0")"
echo "$*" >> "$D/calls"
case "$1" in
apply)
  if [[ "$*" == *"preemptible=false"* ]]; then echo %q; exit %d; fi
  echo %q; exit 1;;
output)
  echo '{"media_endpoint":{"value":"http://1.2.3.4:8080"},"preemptible":{"value":"false"}}';;
esac
`, second, code, first)
	bin = filepath.Join(dir, "terraform")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir, func() []string {
		data, _ := os.ReadFile(filepath.Join(dir, "calls"))
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}

func fallbackManager(bin, dir string, fault string) (*gpuManager, func() []string) {
	m := &gpuManager{tfBin: bin, tfDir: dir, state: "provisioning", ondemandFallback: true,
		fault: func(string) (string, error) { return fault, nil }}
	var mu sync.Mutex
	var said []string
	m.notify = func(s string) {
		mu.Lock()
		said = append(said, s)
		mu.Unlock()
	}
	return m, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), said...)
	}
}

func TestApplyFallsBackToOnDemand(t *testing.T) {
	bin, dir, calls := fakeTF(t, errorInstance, "Apply complete!", true)
	m, said := fallbackManager(bin, dir, "No valid host was found. There are not enough hosts available.")
	m.runOp("apply")

	got := calls()
	if len(got) != 3 || !strings.HasPrefix(got[0], "apply") || got[2] != "output -json" {
		t.Fatalf("вызовы terraform: %q", got)
	}
	// Вторая попытка пересоздаёт ТОЛЬКО сервер, обычным
	if !strings.Contains(got[1], "-var preemptible=false") || !strings.Contains(got[1], "-replace="+instanceAddr) {
		t.Fatalf("вторая попытка: %q", got[1])
	}
	if st := m.status(); st.State != "running" || !st.OnDemand || st.Endpoint != "http://1.2.3.4:8080" {
		t.Fatalf("статус: %+v", st)
	}
	if s := said(); len(s) != 1 || !strings.Contains(s[0], "Прерываемой карты у Selectel сейчас нет (No valid host") {
		t.Fatalf("владельцу: %q", s)
	}
}

func TestApplyNoFallbackOnQuota(t *testing.T) {
	bin, dir, calls := fakeTF(t, errorInstance, "", true)
	m, said := fallbackManager(bin, dir, "Quota exceeded for cores")
	m.runOp("apply")
	if got := calls(); len(got) != 1 {
		t.Fatalf("на квоте вторая попытка бессмысленна: %q", got)
	}
	if st := m.status(); st.State != "error" || !strings.Contains(st.Err, "Quota exceeded") || len(said()) != 0 {
		t.Fatalf("статус: %+v", st)
	}
}

func TestApplyNoCardsAtAll(t *testing.T) {
	bin, dir, _ := fakeTF(t, errorInstance, errorInstance, false)
	m, _ := fallbackManager(bin, dir, "No valid host was found.")
	m.runOp("apply")
	if st := m.status(); st.State != "error" || !strings.Contains(st.Err, "ни прерываемых, ни обычных") {
		t.Fatalf("статус: %+v", st)
	}
}

func TestApplyFallbackDisabled(t *testing.T) {
	bin, dir, calls := fakeTF(t, errorInstance, "Apply complete!", true)
	m, _ := fallbackManager(bin, dir, "No valid host was found.")
	m.ondemandFallback = false
	m.runOp("apply")
	if got := calls(); len(got) != 1 || m.status().State != "error" {
		t.Fatalf("запасной путь выключен, а вызовы: %q", got)
	}
}

func TestNovaServerFault(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/identity/v3/auth/tokens":
			var body struct {
				Auth struct {
					Identity struct {
						Password struct {
							User struct {
								Name, Password string
								Domain         struct{ Name string }
							}
						}
					}
					Scope struct {
						Project struct{ ID string }
					}
				}
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			u := body.Auth.Identity.Password.User
			if u.Name != "terraform-admin" || u.Password != `pa"ss` || u.Domain.Name != "123456" || body.Auth.Scope.Project.ID != "proj" {
				t.Errorf("вход в keystone: %+v %+v", u, body.Auth.Scope)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("X-Subject-Token", "tok")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": map[string]any{"catalog": []any{
				map[string]any{"type": "network", "endpoints": []any{map[string]any{"interface": "public", "region": "ru-7", "url": srv.URL + "/net"}}},
				map[string]any{"type": "compute", "endpoints": []any{
					map[string]any{"interface": "internal", "region": "ru-7", "url": srv.URL + "/wrong"},
					map[string]any{"interface": "public", "region": "ru-3", "url": srv.URL + "/wrong"},
					map[string]any{"interface": "public", "region_id": "ru-7", "url": srv.URL + "/compute/v2.1/"},
				}},
			}}})
		case "/compute/v2.1/servers/srv-1":
			if r.Header.Get("X-Auth-Token") != "tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"server":{"status":"ERROR","fault":{"code":500,"message":"No valid host was found. There are not enough hosts available."}}}`))
		default:
			t.Errorf("неожиданный запрос %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	vars := filepath.Join(t.TempDir(), "terraform.tfvars")
	_ = os.WriteFile(vars, []byte(`# креды
selectel_account_id  = "123456"
selectel_username    = "terraform-admin"
selectel_password    = "pa\"ss"
selectel_project_id  = "proj"
selectel_auth_region = "ru-7"
vps_flavor_id        = "0000"
`), 0o600)
	t.Setenv("KRISTINA_OS_AUTH_URL", srv.URL+"/identity/v3/")
	c, err := novaFromTFVars(vars)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := c.serverFault(context.Background(), "srv-1")
	if err != nil || !strings.HasPrefix(msg, "No valid host") {
		t.Fatalf("%q, %v", msg, err)
	}

	t.Setenv("TF_VAR_selectel_project_id", "other")
	if c, _ := novaFromTFVars(vars); c.projectID != "other" {
		t.Fatal("TF_VAR_* должен перекрывать tfvars, как у терраформа")
	}
	_ = os.WriteFile(vars, []byte(`selectel_username = "x"`), 0o600)
	if _, err := novaFromTFVars(vars); err == nil {
		t.Fatal("без всех кредов — ошибка, а не клиент-пустышка")
	}
}

// Упала установка на машине: бот забирает её лог по ssh из output
// ssh_command — тем же ключом, что у терраформа
func TestApplyProvisionerErrorFetchesSetupLog(t *testing.T) {
	dir := t.TempDir()
	tf := filepath.Join(dir, "terraform")
	if err := os.WriteFile(tf, []byte(`#!/bin/bash
case "$1" in
apply) echo 'Error: remote-exec provisioner error'; exit 1;;
output) echo '{"ssh_command":{"value":"ssh -i ~/.ssh/selectel root@90.1.2.3","sensitive":true}}';;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}
	ssh := filepath.Join(dir, "ssh")
	if err := os.WriteFile(ssh, []byte(`#!/bin/bash
echo "$*" > "$(dirname "$0")/ssh-args"
echo '=== kristina-lipsync-base:f84 — из кэша ==='
echo ...
echo 'short read: expected 1378857050 bytes but got 581979136: unexpected EOF'
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", "/root")
	m, _ := fallbackManager(tf, dir, "")
	m.sshBin = ssh
	m.runOp("apply")

	st := m.status()
	if st.State != "error" || !strings.Contains(st.Err, "Лог установки на GPU") || !strings.Contains(st.Err, "short read") {
		t.Fatalf("статус: %+v", st)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "ssh-args"))
	if !strings.HasPrefix(string(args), "-i /root/.ssh/selectel ") || !strings.Contains(string(args), "-o BatchMode=yes") || !strings.Contains(string(args), "root@90.1.2.3 f=/root/setup_kristina.log") {
		t.Fatalf("ssh: %s", args)
	}
}
