#!/bin/bash
# Модель для «мозга» — с Hugging Face прямо в S3, без копии на диске:
# curl стримит файл, aws s3 cp - заливает его кусками по мере скачивания.
# Так GPU-машине не придётся качать 17 GB за свои деньги при подъёме.
#
#   tools/fetch_model.sh                                   # Qwen3.8-27B (по умолчанию)
#   tools/fetch_model.sh <hf-репо> <файл.gguf> [mmproj…]   # любая другая
#
# Кладёт в $KRISTINA_S3_MODELS/<hf-репо>/ — ровно туда, где её ищет
# selectel/ (llm_models_uri + llm_model_path). Нужны curl и aws CLI,
# настройки — tools/s3.env. С мака под Happ в TUN-режиме S3 Selectel
# виснет — запускай с VPS бота.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

REPO="${1:-lmstudio-community/Qwen3.8-27B-GGUF}"
shift || true
FILES=("$@")
[ ${#FILES[@]} -gt 0 ] || FILES=(Qwen3.8-27B-Q4_K_M.gguf mmproj-Qwen3.8-27B-BF16.gguf)
MODELS="${KRISTINA_S3_MODELS:-s3://lmify-models/models}"

s3_init

for f in "${FILES[@]}"; do
  url="https://huggingface.co/$REPO/resolve/main/$f"
  dst="${MODELS%/}/$REPO/$f"
  if s3 s3 ls "$dst" > /dev/null 2>&1; then
    echo "= $f уже в S3"
    continue
  fi
  # Размер — чтобы aws выбрал размер кусков (без него на больших файлах
  # он упирается в лимит 10 000 частей)
  size="$(curl -sfIL "$url" | tr -d '\r' | awk 'tolower($1)=="content-length:"{v=$2} END{print v}')"
  [ -n "$size" ] || die "$url не отвечает — нет такого файла или репозитория?"
  echo "=== $f ($((size / 1024 / 1024)) МБ) → $dst"
  curl -fL --retry 5 --retry-delay 10 "$url" | s3 s3 cp - "$dst" --expected-size "$size"
done

s3 s3 ls "${MODELS%/}/$REPO/" --human-readable
