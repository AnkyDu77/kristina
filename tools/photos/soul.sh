#!/usr/bin/env bash
# Фото Кристины через Soul ID: создать задачу, дождаться, скачать полноразмерный PNG.
#
#   soul.sh <out.png|out.webp> <prompt> [доп. флаги higgsfield, напр. --image ref.png --seed 7 --aspect_ratio 3:4]
#
# Готовый <out.png> не перегенерируется. id задачи кладётся рядом в <out>.job; если он там
# уже есть, скрипт дожидается этой задачи (повторный запуск не платит дважды). Новый дубль — удалить .job.
set -euo pipefail

SOUL_ID=${SOUL_ID:-2a978ec2-087e-4a81-bbdc-8f1d5fb46847}
out=${1:?usage: soul.sh <out.png> <prompt> [flags]}
prompt=${2:?prompt}
shift 2

[ -s "$out" ] && { echo "$out: уже есть"; exit 0; }
mkdir -p "$(dirname "$out")"

model=${MODEL:-text2image_soul_v2}
extra=()
[ "$model" = text2image_soul_v2 ] && extra=(--custom_reference_id "$SOUL_ID")

job="${out%.*}.job"
if [ -s "$job" ]; then
  # задача уже создана прошлым запуском — дожидаемся её, а не платим за новую
  id=$(cat "$job")
else
  id=$(higgsfield generate create "$model" ${extra[@]+"${extra[@]}"} --prompt "$prompt" "$@" --json |
    python3 -c 'import json,sys;print(json.load(sys.stdin)[0])')
  echo "$id" >"$job"
fi

res=$(higgsfield generate wait "$id" --quiet --timeout 15m --json 2>/dev/null) || {
  echo "$out: задача $id не удалась: $(echo "$res" | tail -1)" >&2
  exit 1
}
url=$(echo "$res" | python3 -c 'import json,sys;print(json.load(sys.stdin)["result_url"])')
# <out>.webp — лёгкая копия того же разрешения (_min.webp), в 7–10 раз меньше PNG: CDN бывает медленным
case "$out" in *.webp) url=${url%.png}_min.webp ;; esac
# CDN иногда рвёт загрузку — повторяем и проверяем, что картинка целая
for try in 1 2 3; do
  curl -sf --retry 3 -o "$out.part" "$url" && ffmpeg -v error -i "$out.part" -f null - 2>/dev/null &&
    { mv "$out.part" "$out"; break; }
  rm -f "$out.part"; sleep 2
done
[ -s "$out" ] || { echo "$out: не скачалось ($url)" >&2; exit 1; }
echo "$out: готово"
