package main

// Клиент медиа-API на GPU VPS (nginx → /tts/, /lipsync/). GPU-машина без
// состояния: голос и аватар живут у бота и уезжают на неё по запросу,
// так что её можно сносить и поднимать заново без переноса файлов.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type mediaClient struct {
	key string
	hc  *http.Client
}

func newMediaClient(key string) *mediaClient {
	// Подготовка аватара из лупа — минуты; потолок с запасом
	return &mediaClient{key: key, hc: &http.Client{Timeout: 15 * time.Minute}}
}

// voiceRef — эталон голоса: wav и то, что в нём сказано (клонированию
// нужны оба).
type voiceRef struct {
	WAV  []byte
	Text string
}

type part struct {
	field, name string // name != "" — это файл
	data        []byte
}

func (c *mediaClient) postMultipart(ctx context.Context, url string, parts []part) ([]byte, int, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		if p.name == "" {
			_ = mw.WriteField(p.field, string(p.data))
			continue
		}
		fw, err := mw.CreateFormFile(p.field, p.name)
		if err != nil {
			return nil, 0, err
		}
		if _, err := fw.Write(p.data); err != nil {
			return nil, 0, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, 0, err
	}
	return c.do(ctx, http.MethodPost, url, &buf, mw.FormDataContentType())
}

func (c *mediaClient) do(ctx context.Context, method, url string, body io.Reader, ctype string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, 0, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode/100 != 2 {
		return data, resp.StatusCode, fmt.Errorf("%s %s: %s: %s", method, url, resp.Status,
			strings.TrimSpace(string(data[:min(len(data), 300)])))
	}
	return data, resp.StatusCode, nil
}

// speak — фраза голосом Кристины, wav.
func (c *mediaClient) speak(ctx context.Context, base, text, language string, v voiceRef) ([]byte, error) {
	out, _, err := c.postMultipart(ctx, base+"/tts/speak", []part{
		{field: "text", data: []byte(text)},
		{field: "ref_text", data: []byte(v.Text)},
		{field: "language", data: []byte(language)},
		{field: "ref_audio", name: "ref.wav", data: v.WAV},
	})
	return out, err
}

// design — новый голос по текстовому описанию (Qwen3-TTS VoiceDesign).
func (c *mediaClient) design(ctx context.Context, base, text, instruct, language string) ([]byte, error) {
	body, _ := json.Marshal(map[string]string{"text": text, "instruct": instruct, "language": language})
	out, _, err := c.do(ctx, http.MethodPost, base+"/tts/design", bytes.NewReader(body), "application/json")
	return out, err
}

// ensureAvatar готовит аватар на GPU, если его там ещё нет. id = хэш
// файла: поменял луп — это уже другой аватар, и он подготовится заново.
func (c *mediaClient) ensureAvatar(ctx context.Context, base, path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("аватар: %w", err)
	}
	sum := sha256.Sum256(data)
	id := hex.EncodeToString(sum[:])[:16]

	_, status, err := c.do(ctx, http.MethodGet, base+"/lipsync/avatars/"+id, nil, "")
	if err == nil {
		return id, nil
	}
	if status != http.StatusNotFound {
		return "", err
	}
	_, _, err = c.postMultipart(ctx, base+"/lipsync/avatars", []part{
		{field: "avatar_id", data: []byte(id)},
		{field: "file", name: filepath.Base(path), data: data},
	})
	return id, err
}

// render — кружок: аватар + звук → квадратный mp4.
func (c *mediaClient) render(ctx context.Context, base, avatarID string, wav []byte) ([]byte, error) {
	out, _, err := c.postMultipart(ctx, base+"/lipsync/render", []part{
		{field: "avatar_id", data: []byte(avatarID)},
		{field: "audio", name: "speech.wav", data: wav},
	})
	return out, err
}
