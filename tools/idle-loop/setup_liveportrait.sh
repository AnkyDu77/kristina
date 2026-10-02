#!/bin/bash
# Ставит LivePortrait на мак (Apple Silicon, MPS) — для make_idle.py.
# Отдельно от проекта, в ~/Documents/LivePortrait: гигабайт venv и весов
# не должен уезжать на VPS вместе с kristina/.
#
# Python — 3.12 из uv (тот же, на котором живёт ComfyUI); подойдёт любой 3.10–3.12.
set -euo pipefail
LP="${LIVEPORTRAIT_DIR:-$HOME/Documents/LivePortrait}"
PY="${PYTHON:-$(ls "$HOME"/.local/share/uv/python/cpython-3.12.*-macos-aarch64-none/bin/python3.12 2>/dev/null | head -1)}"
[ -x "$PY" ] || { echo "нужен python 3.12: задай PYTHON=/path/to/python3.12"; exit 1; }

[ -d "$LP" ] || git clone --depth 1 https://github.com/KwaiVGI/LivePortrait.git "$LP"
cd "$LP"
[ -x .venv/bin/python ] || "$PY" -m venv .venv
.venv/bin/pip install -q -U pip
# gradio (веб-морда LivePortrait) не нужен — ставим base-требования без
# него, плюс requests, который обычно приезжает вместе с gradio
REQ="$(mktemp)"
grep -v gradio requirements_base.txt > "$REQ"
.venv/bin/pip install -q torch==2.5.1 torchvision==0.20.1 onnxruntime requests \
  "huggingface_hub>=0.30" -r "$REQ"
rm -f "$REQ"

# Веса для людей (~630 MB): без animal-моделей
.venv/bin/python - <<'PY'
from huggingface_hub import snapshot_download
snapshot_download("KwaiVGI/LivePortrait", local_dir="pretrained_weights",
                  allow_patterns=["insightface/*", "liveportrait/*"],
                  ignore_patterns=["*animal*"])
PY
.venv/bin/python -c "import torch; print('LivePortrait готов, MPS:', torch.backends.mps.is_available())"
