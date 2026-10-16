#!/usr/bin/env bash
# Склейка эпизода «Кристина повторяет тренд»: шоты подгоняются по длительности
# под планы оригинала, звук берётся из оригинала, сверху — надпись.
#
#   compose.sh <episode_dir> [env-файл, по умолчанию episode.env]
#
# В env-файле:
#   SHOTS="shots/s1.mp4 shots/s2.mp4 shots/s3.mp4"   # по порядку
#   TARGETS="5.100 5.033 4.938"                      # длительность каждого плана в оригинале, с
#   AUDIO=src/audio.m4a                              # звук оригинала (пусто — без звука)
#   CAPTION="текст"                                  # надпись (пусто — без неё)
#   CAPTION_END=4.5                                  # до какой секунды держать надпись
#   OUT=out/final.mp4
set -euo pipefail

ep=${1:?usage: compose.sh <episode_dir> [env]}
cd "$ep"
# shellcheck disable=SC1090
source "./${2:-episode.env}"
: "${CAPTION:=}" "${CAPTION_END:=4.5}" "${AUDIO:=}" "${OUT:=out/final.mp4}"

read -r -a shots <<<"$SHOTS"
read -r -a targets <<<"$TARGETS"
[ ${#shots[@]} -eq ${#targets[@]} ] || { echo "SHOTS и TARGETS разной длины" >&2; exit 1; }

mkdir -p "$(dirname "$OUT")" .work
W=720 H=1280 FPS=30

# 1. Каждый шот: 720x1280 (cover), 30 fps, растянут/сжат ровно до целевой длительности.
parts=()
for i in "${!shots[@]}"; do
  src=${shots[$i]} t=${targets[$i]}
  dur=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$src")
  k=$(python3 -c "print($t/$dur)")
  part=.work/part$i.mp4
  ffmpeg -v error -y -i "$src" -an -vf \
    "setpts=PTS*$k,scale=$W:$H:force_original_aspect_ratio=increase,crop=$W:$H,fps=$FPS,tpad=stop_mode=clone:stop_duration=1,trim=duration=$t,setsar=1" \
    -c:v libx264 -crf 16 -pix_fmt yuv420p "$part"
  printf '%s: %.3fs → %ss (×%.3f)\n' "$src" "$dur" "$t" "$k"
  parts+=("$part")
done

# 2. Склейка без перекодирования (параметры у частей одинаковые).
: >.work/list.txt
for p in "${parts[@]}"; do echo "file '$(basename "$p")'" >>.work/list.txt; done
ffmpeg -v error -y -f concat -safe 0 -i .work/list.txt -c copy .work/video.mp4

# 3. Надпись (ASS, плашка как в рилсах) и звук.
vf=null
if [ -n "$CAPTION" ]; then
  end=$(python3 -c "s=float('$CAPTION_END');print(f'0:00:{s:05.2f}')")
  cat >.work/caption.ass <<EOF
[Script Info]
ScriptType: v4.00+
PlayResX: $W
PlayResY: $H

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Cap,Helvetica Neue,38,&H00000000,&H00000000,&H00FFFFFF,&H00FFFFFF,1,0,0,0,100,100,0,0,3,10,0,8,70,70,170,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:00.00,$end,Cap,,0,0,0,,{\\fad(150,250)}$CAPTION
EOF
  vf="ass=.work/caption.ass"
fi

if [ -n "$AUDIO" ]; then
  ffmpeg -v error -y -i .work/video.mp4 -i "$AUDIO" -map 0:v -map 1:a -vf "$vf" \
    -c:v libx264 -crf 18 -pix_fmt yuv420p -c:a aac -b:a 192k -shortest -movflags +faststart "$OUT"
else
  ffmpeg -v error -y -i .work/video.mp4 -vf "$vf" \
    -c:v libx264 -crf 18 -pix_fmt yuv420p -movflags +faststart "$OUT"
fi
echo "→ $OUT ($(ffprobe -v error -show_entries format=duration -of csv=p=0 "$OUT")s)"
