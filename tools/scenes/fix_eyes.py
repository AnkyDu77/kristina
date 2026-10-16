"""Чинит «молочные» глаза в сценах из Kling — LivePortrait, только область глаз.

Когда Кристина переводит взгляд, Kling иногда не двигает зрачок, а
растворяет его и проявляет на новом месте: треть секунды радужка
полупрозрачная, белёсая — выглядит как «рептилоидное» моргание.
Человек переводит взгляд скачком (саккада, ~30–50 мс).

Что делает скрипт с каждым диапазоном --fix A-B (кадры с артефактом):
  - первая половина диапазона получает глаза кадра A-1 (взгляд «до»),
    вторая — кадра B+1 (взгляд «после»): зрачок перескакивает за кадр;
  - поза головы берётся с текущего кадра, а мимика и взгляд — с чистого,
    так что глаза едут вместе с головой;
  - в исходный кадр вклеивается ТОЛЬКО область глаз с мягким краем,
    остальное лицо остаётся от Kling. Кадры вне диапазонов не трогаются.

Найти диапазоны — глазами, при отсмотре клипа (номера кадров видно в
`ffmpeg -vf drawtext=text='%{n}'`). Автоматически их не ищем: по
контрасту глаза молочный зрачок не отличить от пара или смены света.

Запуск — питоном из venv LivePortrait (tools/idle-loop/setup_liveportrait.sh):
  PYTORCH_ENABLE_MPS_FALLBACK=1 ~/Documents/LivePortrait/.venv/bin/python \\
      tools/scenes/fix_eyes.py --source assets/scenes/pilot/kitchen_morning.mp4 \\
      --out assets/scenes/pilot/kitchen_morning_eyes.mp4 --fix 217-225
"""

import argparse
import math
import os
import subprocess
import sys
import time

import cv2
import numpy as np
import torch

# Кадров плавного входа и выхода вклейки по краям диапазона
RAMP = 2


def eye_mask(lmk: np.ndarray, shape) -> np.ndarray:
    """Мягкая маска вокруг обоих глаз с веками, float32 [0,1]."""
    m = np.zeros(shape[:2], np.float32)
    width = 0.0
    # По уголкам глаза (точки 0/12 и 24/36), а не эллипсом по контуру:
    # у прикрытого глаза контур вырождается в линию, и эллипс по нему
    # разворачивается поперёк
    for a, b in ((0, 12), (24, 36)):
        p, q = lmk[a], lmk[b]
        w = float(np.hypot(*(q - p)))
        width = max(width, w)
        ang = math.degrees(math.atan2(q[1] - p[1], q[0] - p[0]))
        cx, cy = (p + q) / 2
        cv2.ellipse(m, (int(cx), int(cy)), (int(w * 0.8), int(w * 0.55)), ang, 0, 360, 1.0, -1)
    k = int(width * 0.35) | 1
    return cv2.GaussianBlur(m, (k, k), 0)


def parse_range(s: str):
    a, _, b = s.partition("-")
    a, b = int(a), int(b or a)
    if a > b:
        raise argparse.ArgumentTypeError(f"диапазон {s}: начало больше конца")
    return a, b


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--liveportrait", default=os.path.expanduser("~/Documents/LivePortrait"))
    ap.add_argument("--source", required=True, help="клип сцены (mp4)")
    ap.add_argument("--out", required=True)
    ap.add_argument("--fix", type=parse_range, action="append", required=True,
                    help="кадры с артефактом, A-B включительно (можно несколько --fix)")
    args = ap.parse_args()

    source, out_path = os.path.abspath(args.source), os.path.abspath(args.out)
    sys.path.insert(0, args.liveportrait)
    os.chdir(args.liveportrait)  # LivePortrait ищет веса относительно себя

    from src.config.crop_config import CropConfig
    from src.config.inference_config import InferenceConfig
    from src.live_portrait_pipeline import LivePortraitPipeline
    from src.utils.crop import paste_back, prepare_paste_back

    pipe = LivePortraitPipeline(inference_cfg=InferenceConfig(), crop_cfg=CropConfig())
    lp = pipe.live_portrait_wrapper

    cap = cv2.VideoCapture(source)
    fps = cap.get(cv2.CAP_PROP_FPS)
    frames = []
    while True:
        ok, fr = cap.read()
        if not ok:
            break
        frames.append(cv2.cvtColor(fr, cv2.COLOR_BGR2RGB))
    n = len(frames)
    h, w = frames[0].shape[:2]
    print(f"{n} кадров {w}x{h} @ {fps:.0f}", flush=True)

    # ── План: для каждого кадра — чьи глаза и с какой силой вклеивать ────
    appear = np.arange(n)
    alpha = np.zeros(n)
    for a, b in args.fix:
        if a < 1 or b > n - 2:
            sys.exit(f"--fix {a}-{b}: нужен чистый кадр до и после (кадры 1..{n - 2})")
        mid = (a + b + 1) // 2
        for j in range(a, b + 1):
            appear[j] = a - 1 if j < mid else b + 1
            alpha[j] = 1.0
        # Вход и выход: кадры за краем диапазона чистые, вклеиваем в них
        # их же глаза, перерисованные LivePortrait, — шва нет
        for d in range(1, RAMP + 1):
            for j in (a - d, b + d):
                if 0 <= j < n:
                    alpha[j] = max(alpha[j], 1.0 - d / (RAMP + 1))
        print(f"кадры {a}–{b} ({a / fps:.2f}–{b / fps:.2f} с): глаза кадров {a - 1} и {b + 1}, "
              f"скачок взгляда на кадре {mid}", flush=True)

    need = sorted(set(np.flatnonzero(alpha)) | {int(appear[i]) for i in np.flatnonzero(alpha)})
    crops = {}
    for i in need:
        crops[i] = pipe.cropper.crop_source_image(frames[i], pipe.cropper.crop_cfg)
        if crops[i] is None:
            sys.exit(f"кадр {i}: лицо не найдено")

    feat = {}

    def source_of(j):
        if j not in feat:
            I = lp.prepare_source(crops[j]["img_crop_256x256"])
            info = lp.get_kp_info(I)
            feat[j] = (lp.extract_feature_3d(I), lp.transform_keypoint(info), info)
        return feat[j]

    # ── Рендер ───────────────────────────────────────────────────────────
    ff = subprocess.Popen(
        ["ffmpeg", "-v", "error", "-y",
         "-f", "rawvideo", "-pix_fmt", "rgb24", "-s", f"{w}x{h}", "-r", f"{fps}", "-i", "-",
         "-c:v", "libx264", "-preset", "slow", "-crf", "16", "-pix_fmt", "yuv420p",
         "-movflags", "+faststart", out_path],
        stdin=subprocess.PIPE,
    )
    t0 = time.time()
    with torch.no_grad():
        for i, fr in enumerate(frames):
            if alpha[i] <= 0:
                ff.stdin.write(np.ascontiguousarray(fr).tobytes())
                continue
            a = int(appear[i])
            f_s, x_s, info_a = source_of(a)
            if a == i:
                x_d = x_s
            else:
                # Поза головы (поворот, масштаб, сдвиг) — текущего кадра,
                # мимика и взгляд — чистого кадра a
                info_i = source_of(i)[2]
                x_d = lp.transform_keypoint({**info_i, "kp": info_a["kp"], "exp": info_a["exp"]})
            x_d = lp.stitching(x_s, x_d)
            out = lp.parse_output(lp.warp_decode(f_s, x_s, x_d)["out"])[0]
            mask_ori = prepare_paste_back(lp.inference_cfg.mask_crop, crops[i]["M_c2o"], dsize=(w, h))
            full = paste_back(out, crops[i]["M_c2o"], fr, mask_ori)
            m = (eye_mask(crops[i]["lmk_crop"], fr.shape) * alpha[i])[..., None]
            ff.stdin.write(np.ascontiguousarray((fr * (1 - m) + full * m).astype(np.uint8)).tobytes())

    ff.stdin.close()
    if ff.wait() != 0:
        sys.exit("ffmpeg упал")
    print(f"готово: {out_path} ({int((alpha > 0).sum())} кадров за {time.time() - t0:.0f} с)")


if __name__ == "__main__":
    main()
