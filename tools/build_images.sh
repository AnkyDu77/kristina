#!/bin/bash
# Собирает БАЗЫ docker-образов медиа-стека (media/<svc>/base/ — torch,
# mmcv, MuseTalk и прочее тяжёлое) на обычной CPU-машине (amd64) и кладёт
# их в S3-кэш под теми тегами, которые ищет GPU-машина. Тонкий слой с
# server.py GPU-машина собирает сама за секунды.
#
# Скрипт не обязателен: не найдя базу в кэше, GPU-машина соберёт её сама
# и положит в S3. Он лишь экономит деньги за эту первую сборку.
#
#   tools/build_images.sh           собрать то, чего нет в кэше
#   FORCE=1 tools/build_images.sh   пересобрать всё
#   SERVICES=tts tools/build_images.sh   только TTS (lipsync соберёт GPU)
#
# Нужно: linux/amd64, docker, zstd, aws CLI и место на диске: ~35 GB на
# lipsync, ~20 на tts (порог задаёт MIN_FREE_GB).
set -euo pipefail
source "$(dirname "$0")/lib.sh"
MEDIA="$HERE/../media"

[ "$(uname -m)" = x86_64 ] || die "нужна amd64-машина: на ARM (мак M1) образы под CUDA собираются только эмуляцией, часами"
command -v docker >/dev/null || die "нет docker (см. tools/README.md)"
command -v zstd >/dev/null || die "нет zstd: apt install zstd"
s3_init

DOCKER_ROOT="$(docker info -f '{{.DockerRootDir}}' 2>/dev/null || echo /var/lib/docker)"
FREE_GB=$(df -BG --output=avail "$DOCKER_ROOT" | tail -1 | tr -dc 0-9)
MIN_FREE_GB="${MIN_FREE_GB:-35}"
[ "$FREE_GB" -ge "$MIN_FREE_GB" ] || die "под $DOCKER_ROOT свободно ${FREE_GB} GB, нужно ~$MIN_FREE_GB: образы собираются по одному, но распакованный lipsync с кэшем сборки весит ~25 GB"

# Проверки образа без GPU: импортируется ли всё, что нужно сервису, и не
# уехал ли torch на CUDA новее, чем умеет драйвер 535 на образе Selectel
smoke_tts() {
  docker run --rm "$1" python -c '
import torch, torchaudio, transformers
from qwen_tts import Qwen3TTSModel
import fastapi, soundfile
print("torch", torch.__version__, "cuda", torch.version.cuda, "transformers", transformers.__version__)
major, minor = map(int, torch.version.cuda.split(".")[:2])
assert (major, minor) <= (12, 2), "CUDA %s новее, чем драйвер 535 (≤ 12.2)" % torch.version.cuda
'
}

smoke_lipsync() {
  docker run --rm "$1" python3 -c '
import torch, mmcv, mmdet, mmpose, diffusers, transformers
from musetalk.utils.utils import load_all_model, datagen
from musetalk.utils.blending import get_image_blending
from musetalk.utils.audio_processor import AudioProcessor
import fastapi
print("torch", torch.__version__, "cuda", torch.version.cuda, "mmcv", mmcv.__version__)
try:
    import mmcv.ops  # CUDA-расширения mmcv: без драйвера может не импортироваться
    print("mmcv.ops ok")
except Exception as e:
    print("mmcv.ops без GPU не проверить:", e)
'
}

for svc in ${SERVICES:-tts lipsync}; do
  tag="$(image_tag "$MEDIA/$svc/base")"
  img="kristina-$svc-base:$tag"
  key="${KRISTINA_S3_CACHE%/}/images/kristina-$svc-base-$tag.tar.zst"

  if [ "${FORCE:-0}" != 1 ] && s3 s3 ls "$key" >/dev/null 2>&1; then
    echo "=== $img уже в кэше — пропускаю ==="
    continue
  fi

  echo "=== $img: сборка (lipsync — 20–40 минут) ==="
  DOCKER_BUILDKIT=1 docker build --progress=plain -t "$img" "$MEDIA/$svc/base"

  echo "=== $img: проверка ==="
  "smoke_$svc" "$img"

  echo "=== $img → $key ==="
  # --expected-size нужен aws на потоке больше 50 GB; ставим с запасом
  docker save "$img" | zstd -T0 -3 | s3 s3 cp - "$key" --expected-size 60000000000
  s3 s3 ls "$key"

  # Место на маленькой VPS кончается быстро — чистим за собой
  docker rmi "$img" >/dev/null
  docker builder prune -af >/dev/null
done

echo "Готово. В кэше:"
s3 s3 ls "${KRISTINA_S3_CACHE%/}/images/"
