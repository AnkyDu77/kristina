package main

// Минимальный клиент OpenStack — ровно чтобы узнать, ПОЧЕМУ сервер не
// поднялся. Провайдер terraform (openstack 3.4.0) на упавшем сервере пишет
// только «unexpected state 'ERROR', wanted target 'ACTIVE'», а причина —
// «No valid host was found» (нет свободных карт) или что-то другое — лежит
// в поле fault самого сервера. Его и читаем: Keystone (пароль) → токен и
// адрес Nova из каталога → GET /servers/<id>.
//
// Креды — из того же selectel/terraform.tfvars, что у терраформа: второй
// копии секретов в .env бота не заводим.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

type novaClient struct {
	authURL, username, password, domain, projectID, region string
	hc                                                     *http.Client
}

// tfvarRe — строковое присваивание из tfvars: key = "value".
var tfvarRe = regexp.MustCompile(`(?m)^\s*([A-Za-z0-9_]+)\s*=\s*"((?:[^"\\]|\\.)*)"`)

func readTFVars(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	vars := map[string]string{}
	for _, m := range tfvarRe.FindAllStringSubmatch(string(data), -1) {
		vars[m[1]] = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(m[2])
	}
	return vars, nil
}

// novaFromTFVars — клиент по кредам терраформа; TF_VAR_* перекрывают файл,
// как и у самого терраформа.
func novaFromTFVars(path string) (*novaClient, error) {
	vars, err := readTFVars(path)
	if err != nil {
		return nil, err
	}
	get := func(k string) string {
		if v := os.Getenv("TF_VAR_" + k); v != "" {
			return v
		}
		return vars[k]
	}
	c := &novaClient{
		authURL:   envStr("KRISTINA_OS_AUTH_URL", "https://cloud.api.selcloud.ru/identity/v3"),
		username:  get("selectel_username"),
		password:  get("selectel_password"),
		domain:    get("selectel_account_id"),
		projectID: get("selectel_project_id"),
		region:    get("selectel_auth_region"),
		hc:        &http.Client{Timeout: 30 * time.Second},
	}
	if c.username == "" || c.password == "" || c.domain == "" || c.projectID == "" || c.region == "" {
		return nil, fmt.Errorf("%s: нет selectel_username/password/account_id/project_id/auth_region", path)
	}
	return c, nil
}

// auth — токен проекта и адрес Nova из каталога.
func (c *novaClient) auth(ctx context.Context) (token, compute string, err error) {
	body, _ := json.Marshal(map[string]any{"auth": map[string]any{
		"identity": map[string]any{
			"methods": []string{"password"},
			"password": map[string]any{"user": map[string]any{
				"name": c.username, "password": c.password, "domain": map[string]any{"name": c.domain},
			}},
		},
		"scope": map[string]any{"project": map[string]any{"id": c.projectID}},
	}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.authURL, "/")+"/auth/tokens", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("keystone: %s", resp.Status)
	}
	var out struct {
		Token struct {
			Catalog []struct {
				Type      string `json:"type"`
				Endpoints []struct {
					Interface string `json:"interface"`
					Region    string `json:"region"`
					RegionID  string `json:"region_id"`
					URL       string `json:"url"`
				} `json:"endpoints"`
			} `json:"catalog"`
		} `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return "", "", fmt.Errorf("keystone: %w", err)
	}
	for _, s := range out.Token.Catalog {
		if s.Type != "compute" {
			continue
		}
		for _, e := range s.Endpoints {
			if e.Interface == "public" && (e.Region == c.region || e.RegionID == c.region) {
				return resp.Header.Get("X-Subject-Token"), strings.TrimRight(e.URL, "/"), nil
			}
		}
	}
	return "", "", fmt.Errorf("keystone: в каталоге нет compute для %s", c.region)
}

// serverFault — почему сервер в ERROR: fault.message, как его записал Nova.
// Пусто — сервера нет или причина не записана.
func (c *novaClient) serverFault(ctx context.Context, id string) (string, error) {
	token, compute, err := c.auth(ctx)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, compute+"/servers/"+id, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Auth-Token", token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("nova: %s", resp.Status)
	}
	var out struct {
		Server struct {
			Status string `json:"status"`
			Fault  *struct {
				Message string `json:"message"`
			} `json:"fault"`
		} `json:"server"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("nova: %w", err)
	}
	if out.Server.Fault == nil {
		return "", nil
	}
	return strings.TrimSpace(out.Server.Fault.Message), nil
}

// ── Нет карт или что-то другое ─────────────────────────────────────────────

var (
	// Сервер создался и упал в ERROR — номер его ищем в логе терраформа
	instErrRe = regexp.MustCompile(`Error waiting for instance \(([0-9a-fA-F-]{36})\) to become ready: unexpected state 'ERROR'`)
	// Так Nova и Selectel говорят «свободных хостов (карт) нет»
	capacityRe = regexp.MustCompile(`(?i)no valid host|not enough hosts|NoValidHost|insufficient (resources|capacity)|` +
		`out of capacity|no (free )?capacity|недостаточно (свободных )?ресурсов|нет (свободных )?ресурсов`)
	// Квота — не нехватка карт: обычный сервер в неё упрётся точно так же
	quotaRe = regexp.MustCompile(`(?i)quota|квот`)
)

// capacityVerdict — упал ли apply из-за того, что Selectel не выдал карту.
// reason — что показать человеку.
func capacityVerdict(tfOut string, fault func(id string) (string, error)) (capacity bool, reason string) {
	if quotaRe.MatchString(tfOut) {
		return false, "упёрлись в квоту проекта"
	}
	if m := capacityRe.FindString(tfOut); m != "" {
		return true, m
	}
	id := instErrRe.FindStringSubmatch(tfOut)
	if id == nil {
		return false, "" // упало не на сервере — сеть, диск, провижининг
	}
	msg := ""
	if fault != nil {
		var err error
		if msg, err = fault(id[1]); err != nil {
			msg = ""
		}
	}
	switch {
	case msg == "":
		// Сервер с проверенными образом и флейвором уходит в ERROR при
		// создании почти всегда из-за места. Перестраховаться дешевле:
		// не так — вторая попытка просто упадёт с той же ошибкой
		return true, "сервер ушёл в ERROR, причину Selectel не сообщил"
	case quotaRe.MatchString(msg):
		return false, msg
	case capacityRe.MatchString(msg):
		return true, msg
	}
	return false, msg
}
