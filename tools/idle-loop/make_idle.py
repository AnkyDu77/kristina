"""Idle-луп Кристины из одного фото — LivePortrait, без видео-драйвера.

Движение не переносится с чужого видео, а генерируется: лёгкие
покачивания головы (сумма синусоид с целым числом периодов на луп —
поэтому он замыкается без шва), «дыхание» масштабом, моргания и
блуждание взгляда. Выражение лица берётся с фото как есть, рот остаётся
закрытым — дальше губами займётся MuseTalk.

Математика повторяет execute_image_retargeting из LivePortrait
(src/gradio_pipeline.py), только параметры меняются по кадрам.

Запуск — питоном из venv LivePortrait (см. tools/idle-loop/README.md):
  ~/Documents/LivePortrait/.venv/bin/python make_idle.py \
      --source ../../assets/face.png --out ../../assets/idle.mp4
"""

import argparse
import math
import os
import random
import subprocess
import sys
import time

import numpy as np
import torch


def harmonics(rng: random.Random, n_harm: int = 3):
    """Плавная периодическая кривая на [0,1) с размахом ±1: сумма синусоид
    с периодами 1, 1/2, 1/3 луп-а и случайными фазами."""
    amps = [1.0, 0.5, 0.25][:n_harm]
    phases = [rng.uniform(0, 2 * math.pi) for _ in amps]
    norm = sum(amps)

    def f(u: float) -> float:
        return sum(a * math.sin(2 * math.pi * (k + 1) * u + p)
                   for k, (a, p) in enumerate(zip(amps, phases))) / norm
    return f


def blink_curve(n_frames: int, fps: int, n_blinks: int, rng: random.Random):
    """b[t] ∈ [0,1]: 0 — глаза как на фото, 1 — закрыты. Моргание
    человека ~0.25 с: быстро закрыть (≈3 кадра), медленнее открыть (≈4)."""
    b = np.zeros(n_frames)
    close_n, open_n = max(2, round(0.10 * fps)), max(3, round(0.16 * fps))
    span = close_n + open_n
    # Моргания разносим по лупу равномерно с небольшим разбросом и
    # держим подальше от краёв, чтобы не резать их швом
    slot = n_frames / max(1, n_blinks)
    for i in range(n_blinks):
        start = int(i * slot + rng.uniform(0.2, 0.6) * slot)
        start = min(max(start, 2), n_frames - span - 2)
        for j in range(close_n):
            b[start + j] = 0.5 - 0.5 * math.cos(math.pi * (j + 1) / close_n)
        for j in range(open_n):
            b[start + close_n + j] = 0.5 + 0.5 * math.cos(math.pi * (j + 1) / open_n)
    return b


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--liveportrait", default=os.path.expanduser("~/Documents/LivePortrait"))
    ap.add_argument("--source", required=True, help="фото: лицо анфас, рот закрыт")
    ap.add_argument("--out", required=True, help="mp4 на выходе")
    ap.add_argument("--seconds", type=float, default=8.0)
    ap.add_argument("--fps", type=int, default=25, help="25 — на нём обучен MuseTalk")
    ap.add_argument("--size", type=int, default=720, help="сторона квадрата на выходе")
    ap.add_argument("--head", type=float, default=1.0, help="множитель амплитуды движений головы")
    ap.add_argument("--blinks", type=int, default=3)
    ap.add_argument("--seed", type=int, default=7, help="другой seed — другое движение")
    ap.add_argument("--preview-every", type=int, default=0, help="сохранять каждый N-й кадр в PNG рядом с --out")
    args = ap.parse_args()

    source = os.path.abspath(args.source)
    out_path = os.path.abspath(args.out)
    sys.path.insert(0, args.liveportrait)

    from src.config.crop_config import CropConfig
    from src.config.inference_config import InferenceConfig
    from src.live_portrait_pipeline import LivePortraitPipeline
    from src.utils.camera import get_rotation_matrix
    from src.utils.crop import paste_back, prepare_paste_back
    from src.utils.io import load_image_rgb
    from src.utils.retargeting_utils import calc_eye_close_ratio

    pipe = LivePortraitPipeline(inference_cfg=InferenceConfig(), crop_cfg=CropConfig())
    lp = pipe.live_portrait_wrapper
    dev = lp.device
    print(f"LivePortrait на {dev}", flush=True)

    # ── Исходник: кроп лица, ключевые точки, признаки ─────────────────────
    img_rgb = load_image_rgb(source)
    h, w = img_rgb.shape[:2]
    img_rgb = img_rgb[: h - h % 2, : w - w % 2]  # чётные стороны — для x264
    h, w = img_rgb.shape[:2]

    crop = pipe.cropper.crop_source_image(img_rgb, pipe.cropper.crop_cfg)
    if crop is None:
        sys.exit("лицо на фото не найдено")
    I_s = lp.prepare_source(crop["img_crop_256x256"])
    lmk = crop["lmk_crop"]
    mask_ori = prepare_paste_back(lp.inference_cfg.mask_crop, crop["M_c2o"], dsize=(w, h))

    with torch.no_grad():
        x_s_info = lp.get_kp_info(I_s)
        f_s = lp.extract_feature_3d(I_s)
        x_s = lp.transform_keypoint(x_s_info)
    x_c_s = x_s_info["kp"].to(dev)
    exp_s = x_s_info["exp"].to(dev)
    scale_s = x_s_info["scale"].to(dev)
    t_s = x_s_info["t"].to(dev)
    p_s, y_s, r_s = x_s_info["pitch"], x_s_info["yaw"], x_s_info["roll"]
    R_s = get_rotation_matrix(p_s, y_s, r_s).to(dev)
    eye_src = float(calc_eye_close_ratio(lmk[None]).mean())
    print(f"поза на фото: pitch={p_s.item():.1f} yaw={y_s.item():.1f} roll={r_s.item():.1f}, "
          f"открытость глаз {eye_src:.2f}", flush=True)

    # ── Движение ──────────────────────────────────────────────────────────
    n = round(args.seconds * args.fps)
    rng = random.Random(args.seed)
    pitch_f, yaw_f, roll_f = harmonics(rng), harmonics(rng), harmonics(rng)
    gaze_x, gaze_y = harmonics(rng, 2), harmonics(rng, 2)
    blink = blink_curve(n, args.fps, args.blinks, rng)
    # Градусы: живой человек, который просто смотрит в камеру, двигает
    # головой на единицы градусов — больше уже «кивает»
    A_PITCH, A_YAW, A_ROLL = 1.6 * args.head, 2.4 * args.head, 1.0 * args.head

    # ── Рендер: кадры трубой в ffmpeg ─────────────────────────────────────
    side = min(h, w)
    ff = subprocess.Popen(
        ["ffmpeg", "-v", "error", "-y",
         "-f", "rawvideo", "-pix_fmt", "rgb24", "-s", f"{w}x{h}", "-r", str(args.fps), "-i", "-",
         "-vf", f"crop={side}:{side},scale={args.size}:{args.size}:flags=lanczos",
         "-c:v", "libx264", "-preset", "slow", "-crf", "16", "-pix_fmt", "yuv420p",
         "-movflags", "+faststart", out_path],
        stdin=subprocess.PIPE,
    )
    preview_dir = os.path.splitext(out_path)[0] + "_frames"
    if args.preview_every:
        os.makedirs(preview_dir, exist_ok=True)

    t0 = time.time()
    with torch.no_grad():
        for i in range(n):
            u = i / n
            dp = A_PITCH * pitch_f(u)
            dy = A_YAW * yaw_f(u)
            dr = A_ROLL * roll_f(u)
            R_d = get_rotation_matrix(p_s + dp, y_s + dy, r_s + dr).to(dev)
            R_d_new = (R_d @ R_s.permute(0, 2, 1)) @ R_s

            delta = exp_s.clone()
            # Взгляд гуляет чуть-чуть — как в update_delta_new_eyeball_direction
            ex, ey = 2.5 * gaze_x(u), 1.5 * gaze_y(u)
            k_x = (0.0007, 0.001) if ex > 0 else (0.001, 0.0007)
            delta[0, 11, 0] += ex * k_x[0]
            delta[0, 15, 0] += ex * k_x[1]
            delta[0, 11, 1] += ey * -0.001
            delta[0, 15, 1] += ey * -0.001

            # «Дыхание»: масштаб на доли процента, два вдоха за луп
            breath = 1.0 + 0.004 * math.sin(2 * math.pi * 2 * u)
            x_d = breath * scale_s * (x_c_s @ R_d_new + delta) + t_s

            # Глаза ретаргетим на каждом кадре, а не только во время
            # моргания: иначе на его границах был бы скачок
            target = max(0.02, eye_src * (1.0 - blink[i]))
            eye_ratio = lp.calc_combined_eye_ratio([[target]], lmk)
            x_d = x_d + lp.retarget_eye(x_s, eye_ratio)

            x_d = lp.stitching(x_s, x_d)
            out = lp.parse_output(lp.warp_decode(f_s, x_s, x_d)["out"])[0]
            frame = paste_back(out, crop["M_c2o"], img_rgb, mask_ori)
            ff.stdin.write(np.ascontiguousarray(frame).tobytes())

            if args.preview_every and i % args.preview_every == 0:
                import cv2
                cv2.imwrite(os.path.join(preview_dir, f"{i:04d}.png"), cv2.cvtColor(frame, cv2.COLOR_RGB2BGR))
            if i % 25 == 0:
                print(f"кадр {i}/{n}  {time.time() - t0:.0f} с", flush=True)

    ff.stdin.close()
    if ff.wait() != 0:
        sys.exit("ffmpeg упал")
    print(f"готово: {out_path} ({n} кадров за {time.time() - t0:.0f} с)")


if __name__ == "__main__":
    main()
