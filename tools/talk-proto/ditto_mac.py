"""Ditto (antgroup/ditto-talkinghead, PyTorch-вариант) на маке: фото + речь →
говорящая голова целиком — губы, мимика и движения головы из звука.

Ditto рассчитан на CUDA. Код не трогаем, две подмены делаем здесь:
  1. в конфиге (pkl) все device="cuda" → --device (mps или cpu). ONNX-модели
     при этом сами уходят на CPUExecutionProvider;
  2. модуль LMDM (диффузия движения) device из конфига не получает вовсе и
     создаётся с device='cuda' по умолчанию — меняем ему этот умолчательный.

Запуск — питоном из venv Ditto:
  ~/Documents/ditto-talkinghead/.venv/bin/python ditto_mac.py \
      --source ../../assets/face.png --audio speech.wav --out out/ditto.mp4
"""

import argparse
import os
import pickle
import sys
import tempfile
import time


def patch_devices(obj, device):
    if isinstance(obj, dict):
        return {k: (device if k == "device" and v == "cuda" else patch_devices(v, device))
                for k, v in obj.items()}
    if isinstance(obj, list):
        return [patch_devices(v, device) for v in obj]
    return obj


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--ditto", default=os.path.expanduser("~/Documents/ditto-talkinghead"))
    ap.add_argument("--source", required=True)
    ap.add_argument("--audio", required=True, help="wav; Ditto сам ресемплит в 16 kHz")
    ap.add_argument("--out", required=True)
    ap.add_argument("--device", default="mps", choices=["mps", "cpu"])
    ap.add_argument("--seed", type=int, default=1024)
    args = ap.parse_args()

    source, audio, out = map(os.path.abspath, (args.source, args.audio, args.out))
    os.makedirs(os.path.dirname(out), exist_ok=True)
    os.chdir(args.ditto)  # Ditto ищет часть путей относительно своего каталога
    sys.path.insert(0, args.ditto)

    # Класс, а не модуль: __init__ пакета делает `from .LMDM import LMDM`,
    # и имя модуля в пакете перекрыто одноимённым классом
    from core.models.modules import LMDM
    orig_init = LMDM.__init__

    def lmdm_init(self, *a, device=args.device, **k):
        orig_init(self, *a, device=device, **k)
    LMDM.__init__ = lmdm_init

    with open("checkpoints/ditto_cfg/v0.4_hubert_cfg_pytorch.pkl", "rb") as f:
        cfg = patch_devices(pickle.load(f), args.device)
    tmp_cfg = tempfile.NamedTemporaryFile(suffix=".pkl", delete=False)
    pickle.dump(cfg, tmp_cfg)
    tmp_cfg.close()

    import inference  # из каталога Ditto
    from stream_pipeline_offline import StreamSDK

    inference.seed_everything(args.seed)
    t0 = time.time()
    sdk = StreamSDK(tmp_cfg.name, "checkpoints/ditto_pytorch")
    print(f"модели загружены за {time.time() - t0:.0f} с на {args.device}", flush=True)
    t1 = time.time()
    inference.run(sdk, audio, source, out)
    print(f"готово: {out} — рендер {time.time() - t1:.0f} с", flush=True)
    os.unlink(tmp_cfg.name)


if __name__ == "__main__":
    main()
