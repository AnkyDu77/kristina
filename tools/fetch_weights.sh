#!/bin/bash
# Скачивает все веса медиа-стека Кристины и заливает их в S3-кэш, откуда
# их забирает GPU-машина при подъёме (setup-скрипт selectel/main.tf).
#
#   tools/fetch_weights.sh            скачать + залить
#   tools/fetch_weights.sh --no-upload   только скачать (проверить, что всё есть)
#
# Работает на маке и на linux. Нужно ~14 GB на диске, python3, curl, aws CLI.
# Повторный запуск докачивает недостающее и ничего не качает заново.
set -euo pipefail
source "$(dirname "$0")/lib.sh"

UPLOAD=1
[ "${1:-}" = "--no-upload" ] && UPLOAD=0
STAGE="${KRISTINA_WEIGHTS_STAGE:-$HERE/weights-staging}"
VENV="$HERE/.venv-weights"

# S3 проверяем до скачивания: узнать про неверные ключи после 14 GB обидно
[ "$UPLOAD" = 1 ] && s3_init

echo "=== Python-окружение для скачивания ($VENV) ==="
# Только huggingface_hub и gdown — никакого torch
[ -x "$VENV/bin/python" ] || python3 -m venv "$VENV"
"$VENV/bin/pip" install -q -U pip "huggingface_hub>=0.30" gdown

echo "=== HuggingFace → $STAGE ==="
"$VENV/bin/python" - "$STAGE" <<'PY'
import os, sys
from huggingface_hub import snapshot_download

stage = sys.argv[1]
# (репозиторий, куда положить, какие файлы). Раскладка повторяет то, что
# ждут сервисы: qwen3-tts/<модель>/ и models/ MuseTalk (= musetalk/).
jobs = [
    ("Qwen/Qwen3-TTS-12Hz-1.7B-Base", "qwen3-tts/Qwen3-TTS-12Hz-1.7B-Base", None),
    ("Qwen/Qwen3-TTS-12Hz-1.7B-VoiceDesign", "qwen3-tts/Qwen3-TTS-12Hz-1.7B-VoiceDesign", None),
    ("TMElyralab/MuseTalk", "musetalk", ["musetalkV15/*"]),
    ("stabilityai/sd-vae-ft-mse", "musetalk/sd-vae", ["config.json", "diffusion_pytorch_model.bin"]),
    ("openai/whisper-tiny", "musetalk/whisper", ["config.json", "pytorch_model.bin", "preprocessor_config.json"]),
    ("yzd-v/DWPose", "musetalk/dwpose", ["dw-ll_ucoco_384.pth"]),
]
for repo, dst, allow in jobs:
    print(f"--- {repo} → {dst}", flush=True)
    snapshot_download(repo_id=repo, local_dir=os.path.join(stage, dst), allow_patterns=allow)

# Детектор лица s3fd MuseTalk качает сам при первом импорте — с личного
# сайта автора и в ~/.cache контейнера, то есть заново после каждого
# пересоздания. Кладём его туда, где его найдёт torch.hub: compose задаёт
# TORCH_HOME=models/torch. Копия на HF у LatentSync — тот же файл.
ckpt = os.path.join(stage, "musetalk/torch/hub/checkpoints")
final = os.path.join(ckpt, "s3fd-619a316812.pth")
if not os.path.isfile(final):
    print("--- s3fd (ByteDance/LatentSync-1.5) → musetalk/torch/hub/checkpoints", flush=True)
    tmp = os.path.join(stage, "_s3fd")
    snapshot_download(repo_id="ByteDance/LatentSync-1.5", local_dir=tmp,
                      allow_patterns=["auxiliary/s3fd-619a316812.pth"])
    os.makedirs(ckpt, exist_ok=True)
    os.replace(os.path.join(tmp, "auxiliary/s3fd-619a316812.pth"), final)
PY
rm -rf "$STAGE/_s3fd"

echo "=== face-parse (Google Drive + pytorch.org) ==="
FP="$STAGE/musetalk/face-parse-bisent"
mkdir -p "$FP"
if [ ! -s "$FP/79999_iter.pth" ]; then
  # Самое хрупкое место: у Google Drive лимит скачиваний. Упал — подожди
  # и перезапусти, остальное уже скачано
  "$VENV/bin/gdown" "https://drive.google.com/uc?id=154JgKpzCPW82qINcVieuPH3fZ2e0P812" -O "$FP/79999_iter.pth"
fi
[ -s "$FP/resnet18-5c106cde.pth" ] || \
  curl -fL https://download.pytorch.org/models/resnet18-5c106cde.pth -o "$FP/resnet18-5c106cde.pth"

echo "=== Проверка ==="
missing=0
for f in \
  qwen3-tts/Qwen3-TTS-12Hz-1.7B-Base/model.safetensors \
  qwen3-tts/Qwen3-TTS-12Hz-1.7B-Base/speech_tokenizer/model.safetensors \
  qwen3-tts/Qwen3-TTS-12Hz-1.7B-VoiceDesign/model.safetensors \
  qwen3-tts/Qwen3-TTS-12Hz-1.7B-VoiceDesign/speech_tokenizer/model.safetensors \
  musetalk/musetalkV15/unet.pth musetalk/musetalkV15/musetalk.json \
  musetalk/sd-vae/diffusion_pytorch_model.bin musetalk/sd-vae/config.json \
  musetalk/whisper/pytorch_model.bin musetalk/whisper/preprocessor_config.json \
  musetalk/dwpose/dw-ll_ucoco_384.pth \
  musetalk/face-parse-bisent/79999_iter.pth musetalk/face-parse-bisent/resnet18-5c106cde.pth \
  musetalk/torch/hub/checkpoints/s3fd-619a316812.pth
do
  if [ -s "$STAGE/$f" ]; then
    printf '  ok  %6s  %s\n' "$(du -h "$STAGE/$f" | cut -f1)" "$f"
  else
    printf '  НЕТ         %s\n' "$f"; missing=1
  fi
done
[ "$missing" = 0 ] || die "не всё скачалось — перезапусти скрипт"
du -sh "$STAGE"

if [ "$UPLOAD" = 1 ]; then
  echo "=== Заливка в ${KRISTINA_S3_CACHE%/}/weights ==="
  # .cache/ — служебные метаданные huggingface_hub, на GPU не нужны
  s3 s3 sync "$STAGE" "${KRISTINA_S3_CACHE%/}/weights" --exclude "*.cache/*" --exclude ".cache/*" --no-verify-ssl
  s3 s3 ls "${KRISTINA_S3_CACHE%/}/weights/" --recursive --summarize | tail -2
  echo "Готово. Локальную копию можно удалить: rm -rf '$STAGE'"
fi
