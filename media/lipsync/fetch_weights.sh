#!/bin/bash
# Веса MuseTalk 1.5 — по мотивам download_weights.sh из репозитория, но
# только то, что нужно realtime-инференсу v1.5 (без v1.0 и SyncNet),
# и без hf-mirror.com: из Selectel оригинальный HF доступен.
# Запускается внутри контейнера lipsync (setup-скрипт терраформа делает
# это сам, если весов нет ни на диске, ни в S3-кэше).
set -euo pipefail
cd /app/MuseTalk
D=models
mkdir -p $D/musetalkV15 $D/dwpose $D/face-parse-bisent $D/sd-vae $D/whisper

# huggingface_hub и gdown уже в образе (requirements.txt MuseTalk).
# Не обновлять: hub закреплён на 0.30.2 под transformers 4.39, а в
# новых версиях huggingface-cli заменён на hf.

huggingface-cli download TMElyralab/MuseTalk --local-dir $D \
  --include "musetalkV15/musetalk.json" "musetalkV15/unet.pth"
huggingface-cli download stabilityai/sd-vae-ft-mse --local-dir $D/sd-vae \
  --include "config.json" "diffusion_pytorch_model.bin"
huggingface-cli download openai/whisper-tiny --local-dir $D/whisper \
  --include "config.json" "pytorch_model.bin" "preprocessor_config.json"
huggingface-cli download yzd-v/DWPose --local-dir $D/dwpose \
  --include "dw-ll_ucoco_384.pth"

# face-parse лежит на Google Drive — самое хрупкое место. Если gdown
# упрётся в лимит, положи 79999_iter.pth в S3-кэш руками:
#   s3://<бакет>/cache/weights/musetalk/face-parse-bisent/79999_iter.pth
[ -f $D/face-parse-bisent/79999_iter.pth ] || \
  gdown --id 154JgKpzCPW82qINcVieuPH3fZ2e0P812 -O $D/face-parse-bisent/79999_iter.pth
curl -fsSL https://download.pytorch.org/models/resnet18-5c106cde.pth \
  -o $D/face-parse-bisent/resnet18-5c106cde.pth

# Детектор лица s3fd: иначе MuseTalk скачает его сам в ~/.cache контейнера
# (TORCH_HOME в docker-compose.yml указывает сюда)
mkdir -p $D/torch/hub/checkpoints
curl -fsSL https://huggingface.co/ByteDance/LatentSync-1.5/resolve/main/auxiliary/s3fd-619a316812.pth \
  -o $D/torch/hub/checkpoints/s3fd-619a316812.pth

echo "MuseTalk weights ready"
