"""Стереть мелкие предметы нейросетью LaMa (big-lama.pt, TorchScript) локально на CPU.

    python lama.py <in.jpg> <out.jpg> <эллипс> [<эллипс> ...]

Эллипс — "cx,cy,ax,ay": центр и полуоси области, которую надо стереть (берите с запасом,
вместе с внутренностью кольца — LaMa дорисует мочку/кожу/волосы сама).
Модель: $LAMA_MODEL или ~/Documents/ComfyUI/models/inpaint/big-lama.pt.
Обрабатывается квадрат вокруг масок (LaMa лучше всего работает ~512 px), результат
вклеивается обратно только в маску с мягким краем.
"""
import os
import sys

import cv2
import numpy as np
import torch

MODEL = os.environ.get("LAMA_MODEL", os.path.expanduser("~/Documents/ComfyUI/models/inpaint/big-lama.pt"))

src, dst, specs = sys.argv[1], sys.argv[2], sys.argv[3:]
img = cv2.imread(src)
h, w = img.shape[:2]
mask = np.zeros((h, w), np.uint8)
for s in specs:
    cx, cy, ax, ay = (int(v) for v in s.split(","))
    cv2.ellipse(mask, (cx, cy), (ax, ay), 0, 0, 360, 255, -1)
mask = cv2.dilate(mask, np.ones((5, 5), np.uint8), iterations=2)

# квадрат ~512 вокруг всех масок, кратный 8
ys, xs = np.nonzero(mask)
side = max(512, int(max(xs.max() - xs.min(), ys.max() - ys.min()) * 1.6))
side = min((side + 7) // 8 * 8, (min(h, w) // 8) * 8)
cx, cy = (xs.min() + xs.max()) // 2, (ys.min() + ys.max()) // 2
x0 = int(np.clip(cx - side // 2, 0, w - side))
y0 = int(np.clip(cy - side // 2, 0, h - side))
crop = img[y0:y0 + side, x0:x0 + side]
cmask = mask[y0:y0 + side, x0:x0 + side]

model = torch.jit.load(MODEL, map_location="cpu").eval()
t_img = torch.from_numpy(cv2.cvtColor(crop, cv2.COLOR_BGR2RGB)).permute(2, 0, 1)[None].float() / 255
t_mask = torch.from_numpy((cmask > 127).astype(np.float32))[None, None]
with torch.no_grad():
    res = model(t_img, t_mask)[0].permute(1, 2, 0).numpy()
res = cv2.cvtColor(np.clip(res * 255, 0, 255).astype(np.uint8), cv2.COLOR_RGB2BGR)

a = cv2.GaussianBlur(cmask, (0, 0), 2).astype(np.float32)[..., None] / 255
out = img.copy()
out[y0:y0 + side, x0:x0 + side] = (res * a + crop * (1 - a)).astype(np.uint8)
cv2.imwrite(dst, out, [cv2.IMWRITE_JPEG_QUALITY, 95])
print(dst, f"crop {side}px at ({x0},{y0}), masked {int((cmask > 127).sum())} px")
