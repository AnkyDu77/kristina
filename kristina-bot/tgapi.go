package main

// Тонкий клиент Telegram Bot API (по мотивам lmify-ui/tgapi.go): ровно
// те методы, что нужны Кристине. Длинный опрос, а не вебхук — не нужен
// ни публичный адрес, ни сертификаты, и бот одинаково живёт на ноутбуке
// и на VPS.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

var tgAPIBase = "https://api.telegram.org"

type tgAPI struct {
	token string
	hc    *http.Client // обычные вызовы
	poll  *http.Client // длинный опрос: таймаут больше времени ожидания
	files *http.Client // загрузка кружков и аудио
}

func newTGAPI(token string) *tgAPI {
	return &tgAPI{
		token: token,
		hc:    &http.Client{Timeout: 60 * time.Second},
		poll:  &http.Client{Timeout: 90 * time.Second},
		files: &http.Client{Timeout: 5 * time.Minute},
	}
}

type tgUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type tgMessage struct {
	MessageID int64              `json:"message_id"`
	From      *tgUser            `json:"from"`
	Chat      struct{ ID int64 } `json:"chat"`
	Text      string             `json:"text"`
}

type tgUpdate struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message"`
}

// call выполняет метод Bot API. 429 обрабатываем отдельно: телеграм сам
// говорит, сколько ждать.
func (a *tgAPI) call(ctx context.Context, method string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/bot%s/%s", tgAPIBase, a.token, method)
	client := a.hc
	if method == "getUpdates" {
		client = a.poll
	}

	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		retry, err := a.do(ctx, client, req, method, out)
		if !retry {
			return err
		}
	}
	return fmt.Errorf("%s: не удалось после повторов", method)
}

// do отправляет запрос и разбирает конверт ответа. retry=true — упёрлись
// в лимит частоты, выждали, можно повторять.
func (a *tgAPI) do(ctx context.Context, client *http.Client, req *http.Request, method string, out any) (retry bool, err error) {
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()

	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		Params      struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return false, fmt.Errorf("%s: битый ответ: %s", method, strings.TrimSpace(string(raw[:min(len(raw), 200)])))
	}
	if envelope.OK {
		if out != nil && len(envelope.Result) > 0 {
			return false, json.Unmarshal(envelope.Result, out)
		}
		return false, nil
	}
	if resp.StatusCode == http.StatusTooManyRequests && envelope.Params.RetryAfter > 0 {
		wait := time.Duration(envelope.Params.RetryAfter+1) * time.Second
		log.Printf("tg: %s — лимит частоты, ждём %s", method, wait)
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(wait):
		}
		return true, nil
	}
	return false, fmt.Errorf("%s: %s", method, envelope.Description)
}

func (a *tgAPI) getUpdates(ctx context.Context, offset int64) ([]tgUpdate, error) {
	var out []tgUpdate
	err := a.call(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         50,
		"allowed_updates": []string{"message"},
	}, &out)
	return out, err
}

// deleteWebhook обязателен перед опросом: если у бота стоит вебхук,
// getUpdates отвечает 409 и бот молчит.
func (a *tgAPI) deleteWebhook(ctx context.Context) error {
	return a.call(ctx, "deleteWebhook", map[string]any{"drop_pending_updates": false}, nil)
}

func (a *tgAPI) getMe(ctx context.Context) (*tgUser, error) {
	var u tgUser
	err := a.call(ctx, "getMe", map[string]any{}, &u)
	return &u, err
}

// sendHTML отправляет уже размеченный (см. tgformat.go) текст.
func (a *tgAPI) sendHTML(ctx context.Context, chatID int64, html string) error {
	return a.call(ctx, "sendMessage", map[string]any{
		"chat_id":              chatID,
		"text":                 html,
		"parse_mode":           "HTML",
		"link_preview_options": map[string]any{"is_disabled": true},
	}, nil)
}

// sendPlain — служебные сообщения бота: без разметки, чтобы случайный
// «<» в тексте ошибки не ронял отправку.
func (a *tgAPI) sendPlain(ctx context.Context, chatID int64, text string) error {
	return a.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": text}, nil)
}

// chatAction показывает «печатает…» / «записывает видео…»; статус живёт
// около 5 секунд, поэтому для долгих операций его надо повторять.
func (a *tgAPI) chatAction(ctx context.Context, chatID int64, action string) {
	_ = a.call(ctx, "sendChatAction", map[string]any{"chat_id": chatID, "action": action}, nil)
}

// sendVideoNote — кружок. Видео должно быть квадратным, не длиннее
// минуты; length — сторона квадрата.
func (a *tgAPI) sendVideoNote(ctx context.Context, chatID int64, mp4 []byte, side int) error {
	return a.sendFile(ctx, "sendVideoNote", "video_note", "kristina.mp4", mp4, map[string]string{
		"chat_id": fmt.Sprint(chatID),
		"length":  fmt.Sprint(side),
	})
}

func (a *tgAPI) sendAudio(ctx context.Context, chatID int64, name string, data []byte, caption string) error {
	return a.sendFile(ctx, "sendAudio", "audio", name, data, map[string]string{
		"chat_id": fmt.Sprint(chatID),
		"caption": caption,
	})
}

// sendFile — общий multipart-путь для методов с файлом в теле.
func (a *tgAPI) sendFile(ctx context.Context, method, field, name string, data []byte, fields map[string]string) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if v != "" {
			_ = mw.WriteField(k, v)
		}
	}
	fw, err := mw.CreateFormFile(field, name)
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	endpoint := fmt.Sprintf("%s/bot%s/%s", tgAPIBase, a.token, method)
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf.Bytes()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		retry, err := a.do(ctx, a.files, req, method, nil)
		if !retry {
			return err
		}
	}
	return fmt.Errorf("%s: не удалось после повторов", method)
}
