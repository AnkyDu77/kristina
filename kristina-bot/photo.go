package main

// Доводка фото для Instagram без внешних сервисов: обрезать в нужную
// пропорцию (Soul умеет 3:4, лента любит 4:5), уменьшить до 1080 по
// ширине и добавить лёгкое зерно — оно снимает «пластиковость»
// генерации. И показать владельцу готовое альбомом.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"math"
	mrand "math/rand/v2"
	"path"
	"strconv"
	"strings"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

type photoOpts struct {
	aspectW, aspectH int
	align            string // center | top | bottom
	width            int
	grain            float64 // σ шума в уровнях 0..255
}

func parseAspect(s string) (int, int, error) {
	if s == "" {
		s = "4:5"
	}
	w, h, ok := strings.Cut(s, ":")
	aw, err1 := strconv.Atoi(strings.TrimSpace(w))
	ah, err2 := strconv.Atoi(strings.TrimSpace(h))
	if !ok || err1 != nil || err2 != nil || aw <= 0 || ah <= 0 || aw > 50 || ah > 50 {
		return 0, 0, fmt.Errorf("пропорция %q: нужно вида 4:5", s)
	}
	return aw, ah, nil
}

// finishPhoto — обрезка по пропорции (по центру/верху/низу), уменьшение до
// width (не увеличиваем), зерно, JPEG 92.
func finishPhoto(data []byte, o photoOpts) ([]byte, image.Point, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, image.Point{}, fmt.Errorf("не картинка (png/jpeg/webp): %w", err)
	}
	b := src.Bounds()
	cw, ch := b.Dx(), b.Dy()
	if cw*o.aspectH > ch*o.aspectW {
		cw = ch * o.aspectW / o.aspectH // шире нужного — режем бока
	} else {
		ch = cw * o.aspectH / o.aspectW // выше — режем верх/низ
	}
	x0 := b.Min.X + (b.Dx()-cw)/2
	y0 := b.Min.Y + (b.Dy()-ch)/2
	switch o.align {
	case "top":
		y0 = b.Min.Y
	case "bottom":
		y0 = b.Max.Y - ch
	}
	crop := image.Rect(x0, y0, x0+cw, y0+ch)

	w := o.width
	if w <= 0 || w > cw {
		w = cw
	}
	h := int(math.Round(float64(w) * float64(o.aspectH) / float64(o.aspectW)))
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, crop, draw.Src, nil)

	if o.grain > 0 {
		rng := mrand.New(mrand.NewPCG(1, 2)) // зерно одинаковое при повторе — правки воспроизводимы
		for i := 0; i < len(dst.Pix); i += 4 {
			n := rng.NormFloat64() * o.grain // одно на пиксель: цветной шум выглядит цифровым
			for c := 0; c < 3; c++ {
				dst.Pix[i+c] = clamp8(float64(dst.Pix[i+c]) + n)
			}
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 92}); err != nil {
		return nil, image.Point{}, err
	}
	return out.Bytes(), image.Pt(w, h), nil
}

func clamp8(v float64) uint8 {
	switch {
	case v < 0:
		return 0
	case v > 255:
		return 255
	}
	return uint8(v + 0.5)
}

func (b *bot) photoTools() []*tool {
	if b.ws == nil {
		return nil
	}
	return []*tool{
		{
			spec: toolSpecFunc{
				Name: "photo_finish",
				Description: "Довести фото для Instagram: обрезать в пропорцию (по умолчанию 4:5), уменьшить до ширины 1080 и добавить лёгкое зерно. " +
					"Бесплатно, локально. Результат — JPEG в out.",
				Parameters: object(map[string]any{
					"path":   str("исходник в /workspace (png/jpg/webp)"),
					"out":    str("куда сохранить, .jpg, например content/2026-10-20-park/final/01.jpg"),
					"aspect": str("пропорция, по умолчанию 4:5 (1:1, 9:16 …)"),
					"align":  map[string]any{"type": "string", "enum": []string{"center", "top", "bottom"}, "description": "что сохранить при обрезке по высоте; top — чтобы не срезать голову/поднятые руки"},
					"width":  map[string]any{"type": "integer", "description": "ширина, по умолчанию 1080"},
					"grain":  map[string]any{"type": "number", "description": "зерно 0–10, по умолчанию 4 (0 — без)"},
				}, "path", "out"),
			},
			modes:  everywhere,
			status: func(a json.RawMessage) string { return "✂️ Довожу " + path.Base(argStr(a, "path")) },
			run: func(_ context.Context, _ int64, a json.RawMessage) (string, error) {
				var args struct {
					Path, Out, Aspect, Align string
					Width                    int
					Grain                    *float64
				}
				if err := json.Unmarshal(a, &args); err != nil {
					return "", err
				}
				aw, ah, err := parseAspect(args.Aspect)
				if err != nil {
					return "", err
				}
				if ext := strings.ToLower(path.Ext(args.Out)); ext != ".jpg" && ext != ".jpeg" {
					return "", errors.New("out — файл .jpg")
				}
				o := photoOpts{aspectW: aw, aspectH: ah, align: args.Align, width: args.Width, grain: 4}
				if o.width == 0 {
					o.width = 1080
				}
				if args.Grain != nil {
					o.grain = math.Max(0, math.Min(10, *args.Grain))
				}
				_, data, err := b.ws.readAll(args.Path, hfMaxDownload)
				if err != nil {
					return "", err
				}
				out, size, err := finishPhoto(data, o)
				if err != nil {
					return "", err
				}
				if err := b.ws.writeBytes(args.Out, out); err != nil {
					return "", err
				}
				return fmt.Sprintf("Готово: %s — %d×%d, %s.", args.Out, size.X, size.Y, humanSize(int64(len(out)))), nil
			},
		},
		{
			spec: toolSpecFunc{
				Name:        "send_photos",
				Description: "Показать владельцу фото из /workspace альбомом (до 10, по порядку слайдов) с подписью — для выбора или как готовую карусель.",
				Parameters: object(map[string]any{
					"paths":   map[string]any{"type": "array", "items": str(""), "minItems": 1, "maxItems": 10},
					"caption": str("подпись к альбому"),
				}, "paths"),
			},
			modes:  everywhere,
			status: func(json.RawMessage) string { return "🖼 Показываю фото" },
			run: func(ctx context.Context, chatID int64, a json.RawMessage) (string, error) {
				paths := argStrings(a, "paths")
				if len(paths) == 0 || len(paths) > 10 {
					return "", errors.New("нужно от 1 до 10 путей")
				}
				var ups []tgUpload
				for _, p := range paths {
					name, data, err := b.ws.readAll(p, tgPhotoMax)
					if err != nil {
						return "", err
					}
					if !hfImageExt[strings.ToLower(path.Ext(name))] {
						return "", fmt.Errorf("%s — не фото", p)
					}
					ups = append(ups, tgUpload{Name: name, Data: data})
				}
				caption := truncRunes(argStr(a, "caption"), 1000)
				var err error
				if len(ups) == 1 {
					err = b.tg.sendPhotoNamed(ctx, chatID, ups[0].Name, ups[0].Data, caption)
				} else {
					err = b.tg.sendMediaGroup(ctx, chatID, ups, caption)
				}
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("Показала владельцу %d фото.", len(ups)), nil
			},
		},
	}
}
