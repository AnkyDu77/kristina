"""TTS-сервис Кристины на Qwen3-TTS.

Сервис без состояния: эталон голоса (wav + его расшифровку) присылает
бот в каждом запросе. Так GPU VPS можно сносить и поднимать заново без
переноса файлов, а голос живёт рядом с ботом. Чтобы не пересчитывать
промпт клонирования на каждую фразу, он кэшируется по хэшу эталона.

Маршруты (за nginx они висят под /tts/):
  GET  /health  — 200, когда модель загружена
  POST /speak   — multipart: text, ref_text, language, ref_audio (wav) → audio/wav
  POST /design  — json {text, instruct, language} → audio/wav
                  (голос по текстовому описанию — так Кристина его и получает)
"""

import hashlib
import io
import os
import tempfile
import threading
from collections import OrderedDict

import soundfile as sf
import torch
from fastapi import FastAPI, File, Form, HTTPException, UploadFile
from fastapi.responses import Response
from pydantic import BaseModel
from qwen_tts import Qwen3TTSModel

WEIGHTS_DIR = os.environ.get("TTS_WEIGHTS_DIR", "/weights/qwen3-tts")


def _resolve(repo_id: str) -> str:
    """Локальная копия из S3-кэша, если есть; иначе — id на HuggingFace."""
    local = os.path.join(WEIGHTS_DIR, repo_id.split("/")[-1])
    return local if os.path.isfile(os.path.join(local, "config.json")) else repo_id


BASE_MODEL = _resolve(os.environ.get("TTS_BASE_MODEL", "Qwen/Qwen3-TTS-12Hz-1.7B-Base"))
DESIGN_MODEL = _resolve(os.environ.get("TTS_DESIGN_MODEL", "Qwen/Qwen3-TTS-12Hz-1.7B-VoiceDesign"))
# flash-attn в образ не ставим: его сборка — полчаса и отдельная боль с
# версиями CUDA. Для коротких фраз sdpa хватает с головой.
ATTN = os.environ.get("TTS_ATTN", "sdpa")
DEFAULT_LANGUAGE = os.environ.get("TTS_LANGUAGE", "Russian")
MAX_TEXT = int(os.environ.get("TTS_MAX_TEXT", "1500"))

app = FastAPI(title="kristina-tts")

_base = None
_design = None
# Одна карта — одна генерация за раз: параллельные запросы только
# дерутся за VRAM и в сумме идут не быстрее
_gpu = threading.Lock()
_prompts: "OrderedDict[str, object]" = OrderedDict()


def _load(model_id):
    return Qwen3TTSModel.from_pretrained(
        model_id,
        device_map="cuda:0",
        dtype=torch.bfloat16,
        attn_implementation=ATTN,
    )


@app.on_event("startup")
def startup():
    global _base
    _base = _load(BASE_MODEL)


def _wav_bytes(wavs, sr) -> bytes:
    buf = io.BytesIO()
    sf.write(buf, wavs[0], sr, format="WAV")
    return buf.getvalue()


def _clone_prompt(ref_audio: bytes, ref_text: str):
    key = hashlib.sha256(ref_audio + ref_text.encode()).hexdigest()
    if key in _prompts:
        _prompts.move_to_end(key)
        return _prompts[key]
    # Модель принимает путь к файлу — отдаём ей временный
    with tempfile.NamedTemporaryFile(suffix=".wav") as f:
        f.write(ref_audio)
        f.flush()
        prompt = _base.create_voice_clone_prompt(ref_audio=f.name, ref_text=ref_text)
    _prompts[key] = prompt
    while len(_prompts) > 8:
        _prompts.popitem(last=False)
    return prompt


@app.get("/health")
def health():
    if _base is None:
        raise HTTPException(503, "model is loading")
    return {"ok": True, "model": BASE_MODEL, "design_loaded": _design is not None}


@app.post("/speak")
def speak(
    text: str = Form(...),
    ref_text: str = Form(...),
    language: str = Form(DEFAULT_LANGUAGE),
    ref_audio: UploadFile = File(...),
):
    text = text.strip()
    if not text:
        raise HTTPException(400, "empty text")
    if len(text) > MAX_TEXT:
        raise HTTPException(413, f"text longer than {MAX_TEXT} chars")
    audio = ref_audio.file.read()
    with _gpu:
        prompt = _clone_prompt(audio, ref_text)
        wavs, sr = _base.generate_voice_clone(
            text=text, language=language, voice_clone_prompt=prompt
        )
    return Response(_wav_bytes(wavs, sr), media_type="audio/wav")


class DesignReq(BaseModel):
    text: str
    instruct: str
    language: str = DEFAULT_LANGUAGE


@app.post("/design")
def design(req: DesignReq):
    global _design
    with _gpu:
        # VoiceDesign нужен разово, пока подбираем голос, — держать его
        # в карте постоянно незачем, грузим по первому запросу
        if _design is None:
            _design = _load(DESIGN_MODEL)
        wavs, sr = _design.generate_voice_design(
            text=req.text, language=req.language, instruct=req.instruct
        )
    return Response(_wav_bytes(wavs, sr), media_type="audio/wav")
