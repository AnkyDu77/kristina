package main

// Сцены для кружков по времени суток. Сцена — короткое бесшовное видео
// (петля), на которое lip-sync накладывает губы под речь. У слота суток
// (утро, день, вечер, ночь) своя активная сцена; нет её — говорим на
// запасном аватаре (KRISTINA_AVATAR).
//
// Сцены Кристина делает сама: кадр (Soul) → петля в Higgsfield
// (circle_scene_make: первый и последний кадр — одно фото) → пробный
// кружок → «да» владельца → circle_scene_set. Порядок и требования к
// кадру — в скилле circles. На GPU сцена кэшируется по хэшу файла,
// поэтому переключение слотов ничего не стоит.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const sceneMaxBytes = 100 << 20

type sceneSlot struct {
	Key, Title string
	From, To   int // часы [From, To) по времени владельца; From > To — через полночь
}

var sceneSlots = []sceneSlot{
	{"morning", "утро", 6, 11},
	{"day", "день", 11, 17},
	{"evening", "вечер", 17, 23},
	{"night", "ночь", 23, 6},
}

func slotAt(t time.Time) sceneSlot {
	h := t.Hour()
	for _, s := range sceneSlots {
		if (s.From < s.To && h >= s.From && h < s.To) || (s.From > s.To && (h >= s.From || h < s.To)) {
			return s
		}
	}
	return sceneSlots[0]
}

func slotByKey(k string) (sceneSlot, bool) {
	k = strings.ToLower(strings.TrimSpace(k))
	for _, s := range sceneSlots {
		if s.Key == k || s.Title == k {
			return s, true
		}
	}
	return sceneSlot{}, false
}

func slotKeys() []string {
	var out []string
	for _, s := range sceneSlots {
		out = append(out, s.Key)
	}
	return out
}

// ── База ────────────────────────────────────────────────────────────────────

type circleScene struct {
	Slot, File, Title string
	Created           time.Time
}

func (s *store) activeScene(slot string) (circleScene, bool) {
	var sc circleScene
	var created int64
	err := s.db.QueryRow(`SELECT slot, file, title, created_at FROM circle_scenes
		WHERE slot = ? AND deleted_at IS NULL ORDER BY id DESC LIMIT 1`, slot).Scan(&sc.Slot, &sc.File, &sc.Title, &created)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("scenes: %v", err)
		}
		return sc, false
	}
	sc.Created = time.Unix(created, 0)
	return sc, true
}

// setScene ставит сцену слоту; прежняя уходит в историю (soft delete).
// file == "" — слот возвращается на запасной аватар.
func (s *store) setScene(slot, file, title string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE circle_scenes SET deleted_at = CURRENT_TIMESTAMP WHERE slot = ? AND deleted_at IS NULL`, slot); err != nil {
		return err
	}
	if file != "" {
		if _, err := tx.Exec(`INSERT INTO circle_scenes(slot, file, title, created_at) VALUES(?, ?, ?, ?)`,
			slot, file, title, time.Now().Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ── Кружок на сцене ─────────────────────────────────────────────────────────

// circleAvatar — видео, на котором говорить сейчас: сцена текущего слота
// или запасной аватар. label — для /status и логов.
func (b *bot) circleAvatar(now time.Time) (name string, data []byte, label string, err error) {
	slot := slotAt(now)
	if b.ws != nil {
		if sc, ok := b.store.activeScene(slot.Key); ok {
			name, data, err := b.ws.readAll(sc.File, sceneMaxBytes)
			if err == nil {
				return name, data, fmt.Sprintf("сцена «%s» (%s)", sceneTitle(sc), slot.Title), nil
			}
			log.Printf("scenes: сцена %s для слота %s не читается (%v) — говорю на запасном аватаре", sc.File, slot.Key, err)
		}
	}
	data, err = os.ReadFile(b.cfg.avatarPath)
	if err != nil {
		return "", nil, "", fmt.Errorf("аватар: %w", err)
	}
	return filepath.Base(b.cfg.avatarPath), data, "запасной аватар " + filepath.Base(b.cfg.avatarPath), nil
}

func sceneTitle(sc circleScene) string {
	if sc.Title != "" {
		return sc.Title
	}
	return path.Base(sc.File)
}

func (b *bot) nowLocal() time.Time {
	if b.cfg.tz != nil {
		return time.Now().In(b.cfg.tz)
	}
	return time.Now()
}

// ── Петля из фото ───────────────────────────────────────────────────────────

const scenePrompt = "Seamless idle loop for a video-message avatar. The camera is completely static. " +
	"She stays almost still and looks into the camera with a calm, warm face, mouth closed and relaxed the whole time — " +
	"she is not speaking, not laughing, not turning her head. Only subtle natural motion: slow breathing, one or two natural blinks, " +
	"a few strands of hair move slightly%s. The background and all objects stay perfectly still and do not change. " +
	"The last frame matches the first frame exactly. Realistic, steady, no cuts."

// sceneHFArgs — запрос в Higgsfield для петли: всё, в чём легко ошибиться
// (одинаковые первый и последний кадр, квадрат, multi_shots), зашито.
func (b *bot) sceneHFArgs(raw json.RawMessage) (json.RawMessage, sceneSlot, error) {
	var a struct {
		Photo, Slot, Motion, Mode string
		Duration                  int
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, sceneSlot{}, err
	}
	slot, ok := slotByKey(a.Slot)
	if !ok {
		return nil, sceneSlot{}, fmt.Errorf("слот — один из: %s", strings.Join(slotKeys(), ", "))
	}
	photo := strings.TrimSpace(a.Photo)
	if photo == "" {
		return nil, slot, errors.New("нужно фото-кадр (путь в /workspace или id задачи Higgsfield)")
	}
	if a.Duration != 10 {
		a.Duration = 5
	}
	if a.Mode != "pro" {
		a.Mode = "std"
	}
	motion := strings.TrimSpace(a.Motion)
	if motion != "" {
		motion = "; also " + strings.TrimSuffix(motion, ".")
	}
	args := hfToolArgs{
		Model:  "kling_o3_flf",
		Prompt: fmt.Sprintf(scenePrompt, motion),
		Params: map[string]any{"duration": float64(a.Duration), "aspect_ratio": "1:1", "mode": a.Mode,
			"sound": "off", "multi_shots": "false"},
		StartImage: photo,
		EndImage:   photo,
		Count:      1,
		Name:       fmt.Sprintf("scenes/%s-%s", slot.Key, b.nowLocal().Format("20060102-1504")),
	}
	out, err := json.Marshal(args)
	return out, slot, err
}

// ── Инструменты ────────────────────────────────────────────────────────────

func (b *bot) sceneTools() []*tool {
	if b.ws == nil {
		return nil
	}
	slotEnum := map[string]any{"type": "string", "enum": slotKeys(),
		"description": "утро 6–11, день 11–17, вечер 17–23, ночь 23–6 (время владельца)"}
	return []*tool{
		{
			spec: toolSpecFunc{
				Name:        "circle_scenes",
				Description: "Сцены твоих кружков: какая стоит на каждом слоте суток и какой слот сейчас.",
				Parameters:  object(map[string]any{}),
			},
			modes: everywhere,
			run: func(context.Context, int64, json.RawMessage) (string, error) {
				now := b.nowLocal()
				var sb strings.Builder
				fmt.Fprintf(&sb, "Сейчас %s — слот «%s».\n", now.Format("15:04"), slotAt(now).Title)
				for _, s := range sceneSlots {
					if sc, ok := b.store.activeScene(s.Key); ok {
						fmt.Fprintf(&sb, "%s (%02d–%02d): «%s» — /workspace/%s\n", s.Key, s.From, s.To, sceneTitle(sc), sc.File)
					} else {
						fmt.Fprintf(&sb, "%s (%02d–%02d): нет сцены — запасной аватар\n", s.Key, s.From, s.To)
					}
				}
				return sb.String(), nil
			},
		},
		{
			spec: toolSpecFunc{
				Name: "circle_preview",
				Description: "Пробный кружок на сцене: сказать text на видео scene и прислать владельцу. Нужна проснувшаяся видеокарта. " +
					"Так владелец оценивает сцену перед circle_scene_set.",
				Parameters: object(map[string]any{
					"scene": str("видео сцены в /workspace (.mp4)"),
					"text":  str("1–2 короткие фразы в характере слота, по-русски"),
				}, "scene", "text"),
			},
			modes:   everywhere,
			timeout: 6 * time.Minute,
			status:  func(json.RawMessage) string { return "🎥 Записываю пробный кружок" },
			run: func(ctx context.Context, chatID int64, a json.RawMessage) (string, error) {
				scene, text := strings.TrimSpace(argStr(a, "scene")), strings.TrimSpace(argStr(a, "text"))
				if !hfVideoExt[strings.ToLower(path.Ext(scene))] {
					return "", errors.New("scene — видео .mp4 из /workspace")
				}
				speech := clampSpeech(text, b.cfg.maxSpeech)
				if speech == "" {
					return "", errors.New("пустой text")
				}
				name, data, err := b.ws.readAll(scene, sceneMaxBytes)
				if err != nil {
					return "", err
				}
				voice, err := b.loadVoice()
				if err != nil {
					return "", errNoVoice
				}
				base, release, ok := b.acquireGPU()
				if !ok {
					return "", errGPUAsleep
				}
				defer release()
				stop := b.keepAction(ctx, chatID, "record_video_note")
				defer stop()
				mp4, err := b.circleOn(ctx, base, speech, voice, name, data)
				if err != nil {
					return "", err
				}
				if err := b.sendCircle(ctx, chatID, mp4); err != nil {
					return "", err
				}
				return "Пробный кружок отправлен владельцу. Спроси, ставим ли сцену на слот.", nil
			},
		},
		{
			spec: toolSpecFunc{
				Name: "circle_scene_set",
				Description: "Поставить сцену на слот суток — дальше кружки в это время пойдут на ней. Только после «да» владельца на пробный кружок. " +
					"scene = \"default\" — вернуть слот на запасной аватар.",
				Parameters: object(map[string]any{
					"slot":  slotEnum,
					"scene": str("видео сцены в /workspace (.mp4) или default"),
					"title": str("короткое имя сцены: «кухня утром»"),
				}, "slot", "scene"),
			},
			modes:  everywhere,
			status: func(a json.RawMessage) string { return "🎬 Ставлю сцену на " + argStr(a, "slot") },
			run: func(_ context.Context, _ int64, a json.RawMessage) (string, error) {
				slot, ok := slotByKey(argStr(a, "slot"))
				if !ok {
					return "", fmt.Errorf("слот — один из: %s", strings.Join(slotKeys(), ", "))
				}
				scene := strings.TrimSpace(argStr(a, "scene"))
				if scene == "default" || scene == "" {
					if err := b.store.setScene(slot.Key, "", ""); err != nil {
						return "", err
					}
					return fmt.Sprintf("Слот «%s» снова на запасном аватаре.", slot.Title), nil
				}
				rel, err := b.ws.rel(scene)
				if err != nil {
					return "", err
				}
				if !hfVideoExt[strings.ToLower(path.Ext(rel))] {
					return "", errors.New("сцена — видео .mp4")
				}
				if _, _, err := b.ws.readAll(rel, sceneMaxBytes); err != nil {
					return "", err
				}
				title := truncRunes(strings.TrimSpace(argStr(a, "title")), 60)
				if err := b.store.setScene(slot.Key, rel, title); err != nil {
					return "", err
				}
				return fmt.Sprintf("Сцена %s стоит на слоте «%s» (%02d–%02d).", rel, slot.Title, slot.From, slot.To), nil
			},
		},
	}
}

// sceneMakeTool — петля из фото через Higgsfield (деньги — с кнопкой и
// лимитами hf_generate).
func (b *bot) sceneMakeTool() *tool {
	return &tool{
		spec: toolSpecFunc{
			Name: "circle_scene_make",
			Description: "Сделать сцену для кружков: бесшовную петлю 1:1 из выбранного владельцем кадра (Higgsfield, Kling, первый и последний кадр — это фото). " +
				"5 кр за 5 с (10 с — 10 кр); владелец подтверждает кнопкой. Потом hf_wait → circle_preview → «да» владельца → circle_scene_set. Порядок — скилл circles.",
			Parameters: object(map[string]any{
				"photo":    str("кадр: путь в /workspace или id задачи Higgsfield; квадратный, лицо по центру, рот закрыт"),
				"slot":     map[string]any{"type": "string", "enum": slotKeys()},
				"motion":   str("что ещё тихо шевелится, по-английски: «thin steam rises slowly from the tea cup» (необязательно)"),
				"duration": map[string]any{"type": "integer", "enum": []int{5, 10}, "description": "секунд, по умолчанию 5"},
			}, "photo", "slot"),
		},
		modes:   everywhere,
		timeout: 5 * time.Minute,
		status: func(a json.RawMessage) string {
			return "🎬 Заказываю сцену для кружков: " + argStr(a, "slot")
		},
		confirm: func(chatID int64, raw json.RawMessage) (string, error) {
			args, slot, err := b.sceneHFArgs(raw)
			if err != nil {
				return "", err
			}
			summary, err := b.hfConfirm(chatID, args)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("🎬 Сцена для кружков — %s\n%s", slot.Title, summary), nil
		},
		run: func(ctx context.Context, chatID int64, raw json.RawMessage) (string, error) {
			args, _, err := b.sceneHFArgs(raw)
			if err != nil {
				return "", err
			}
			res, err := b.hfGenerate(ctx, chatID, args)
			if err != nil {
				return "", err
			}
			return res + "\nКогда петля будет готова — circle_preview на ней, потом спроси владельца и circle_scene_set.", nil
		},
	}
}
