"""Какие фото годятся в аватары: лицо находится? какой поворот головы?
Детектор и оценка позы — из LivePortrait (на них же стоит и Ditto).
Запуск: ~/Documents/LivePortrait/.venv/bin/python angles_check.py ../../assets/angles/*.png
"""
import os
import sys

LP = os.path.expanduser("~/Documents/LivePortrait")
sys.path.insert(0, LP)

from src.config.crop_config import CropConfig
from src.config.inference_config import InferenceConfig
from src.live_portrait_pipeline import LivePortraitPipeline
from src.utils.io import load_image_rgb

pipe = LivePortraitPipeline(inference_cfg=InferenceConfig(), crop_cfg=CropConfig())
lp = pipe.live_portrait_wrapper
print(f"{'файл':<10}{'yaw':>7}{'pitch':>7}{'roll':>7}  вердикт")
for path in sys.argv[1:]:
    name = os.path.basename(path)
    crop = pipe.cropper.crop_source_image(load_image_rgb(path), pipe.cropper.crop_cfg)
    if crop is None:
        print(f"{name:<10}{'—':>7}{'—':>7}{'—':>7}  лицо не найдено")
        continue
    info = lp.get_kp_info(lp.prepare_source(crop["img_crop_256x256"]))
    yaw, pitch, roll = (float(info[k]) for k in ("yaw", "pitch", "roll"))
    # Пороги — эмпирика lip-sync/LivePortrait: до ~30° по yaw уверенно,
    # до ~45° терпимо, дальше второй глаз и угол рта уходят из кадра
    a = max(abs(yaw), abs(pitch))
    verdict = "отлично" if a <= 25 else "годится" if a <= 40 else "рискованно"
    print(f"{name:<10}{yaw:7.1f}{pitch:7.1f}{roll:7.1f}  {verdict}")
