"""Lip-sync-сервис Кристины на MuseTalk 1.5 (realtime-схема).

Логика взята из scripts/realtime_inference.py MuseTalk: всё дорогое —
детекция лица, маски, латенты кадров — считается ОДИН раз на аватар.
На каждый ответ работают только UNet и VAE-декодер, поэтому кружок на
20 секунд рендерится за секунды, а не минуты.

Аватар — луп «молчащей» Кристины (mp4) или просто фото. С фото рот
двигается на неподвижном лице: для первого теста годится, для жизни
нужен луп с морганием и лёгким движением головы.

Отличия от оригинального скрипта: нет input() на повторной подготовке
(сервис не интерактивный), кадры без лица выбрасываются, а не ломают
выравнивание списков, и кадры идут в ffmpeg трубой, а не через PNG.

Маршруты (за nginx они висят под /lipsync/):
  GET  /health              — 200, когда модели загружены
  GET  /avatars/{id}        — 200, если аватар подготовлен; иначе 404
  POST /avatars             — multipart: avatar_id, file (mp4/png/jpg) → подготовка
  POST /render              — multipart: avatar_id, audio (wav) → video/mp4 (квадрат)
"""

import os
import re
import shutil
import subprocess
import tempfile
import threading

import cv2
import numpy as np
import torch
from fastapi import FastAPI, File, Form, HTTPException, UploadFile
from fastapi.responses import Response
from transformers import WhisperModel

from musetalk.utils.audio_processor import AudioProcessor
from musetalk.utils.blending import get_image_blending, get_image_prepare_material
from musetalk.utils.face_parsing import FaceParsing
from musetalk.utils.preprocessing import coord_placeholder, get_landmark_and_bbox
from musetalk.utils.utils import datagen, load_all_model

FPS = 25  # на 25 кадрах MuseTalk обучался
BATCH = int(os.environ.get("LIPSYNC_BATCH", "20"))
EXTRA_MARGIN = int(os.environ.get("LIPSYNC_EXTRA_MARGIN", "10"))
# Аватар держим в RAM целиком; 10 с лупа на 720px — около 600 МБ.
# Кружок всё равно 512px, больше брать незачем.
MAX_SIDE = int(os.environ.get("LIPSYNC_MAX_SIDE", "720"))
OUT_SIDE = int(os.environ.get("LIPSYNC_OUT_SIDE", "512"))
# Telegram не принимает кружки длиннее минуты
MAX_SECONDS = float(os.environ.get("LIPSYNC_MAX_SECONDS", "59"))
CACHE_DIR = os.environ.get("LIPSYNC_CACHE", "/cache/avatars")

app = FastAPI(title="kristina-lipsync")
_gpu = threading.Lock()
_m = {}  # модели: заполняется на старте
_avatars = {}  # avatar_id → dict(frames, coords, latents, masks, mask_boxes)


@app.on_event("startup")
def startup():
    device = torch.device("cuda:0")
    vae, unet, pe = load_all_model(
        unet_model_path="./models/musetalkV15/unet.pth",
        vae_type="sd-vae",
        unet_config="./models/musetalkV15/musetalk.json",
        device=device,
    )
    pe = pe.half().to(device)
    vae.vae = vae.vae.half().to(device)
    unet.model = unet.model.half().to(device)
    dtype = unet.model.dtype
    whisper = WhisperModel.from_pretrained("./models/whisper")
    whisper = whisper.to(device=device, dtype=dtype).eval()
    whisper.requires_grad_(False)
    _m.update(
        device=device,
        vae=vae,
        unet=unet,
        pe=pe,
        dtype=dtype,
        whisper=whisper,
        audio=AudioProcessor(feature_extractor_path="./models/whisper"),
        fp=FaceParsing(left_cheek_width=90, right_cheek_width=90),
        timesteps=torch.tensor([0], device=device),
    )
    os.makedirs(CACHE_DIR, exist_ok=True)


def _check_id(avatar_id: str) -> str:
    # id уходит в путь на диске — только безопасные символы
    if not re.fullmatch(r"[A-Za-z0-9_-]{1,64}", avatar_id):
        raise HTTPException(400, "bad avatar_id")
    return avatar_id


def _cache_path(avatar_id: str) -> str:
    return os.path.join(CACHE_DIR, f"{avatar_id}.pt")


def _get_avatar(avatar_id: str):
    if avatar_id in _avatars:
        return _avatars[avatar_id]
    path = _cache_path(avatar_id)
    if os.path.exists(path):
        av = torch.load(path)
        av["latents"] = [l.to(_m["device"]) for l in av["latents"]]
        _avatars[avatar_id] = av
        return av
    return None


def _prepare(avatar_id: str, src: str):
    """Подготовка аватара — повторяет Avatar.prepare_material из MuseTalk."""
    work = tempfile.mkdtemp(prefix="avatar-")
    try:
        # Кадры через ffmpeg: он одинаково понимает и mp4, и одиночное
        # фото, и сразу ужимает до MAX_SIDE (чётные стороны — для x264)
        scale = f"scale='if(gt(iw,ih),min({MAX_SIDE},iw),-2)':'if(gt(iw,ih),-2,min({MAX_SIDE},ih))'"
        subprocess.run(
            ["ffmpeg", "-v", "error", "-y", "-i", src, "-vf", f"{scale},fps={FPS}",
             os.path.join(work, "%08d.png")],
            check=True,
        )
        imgs = sorted(os.path.join(work, f) for f in os.listdir(work) if f.endswith(".png"))
        if not imgs:
            raise HTTPException(400, "no frames decoded")

        coords, frames = get_landmark_and_bbox(imgs, 0)
        vae = _m["vae"]
        kept_frames, kept_coords, latents = [], [], []
        for bbox, frame in zip(coords, frames):
            if bbox == coord_placeholder:
                continue  # кадр без лица — выбрасываем целиком
            x1, y1, x2, y2 = bbox
            y2 = min(y2 + EXTRA_MARGIN, frame.shape[0])
            crop = cv2.resize(frame[y1:y2, x1:x2], (256, 256), interpolation=cv2.INTER_LANCZOS4)
            latents.append(vae.get_latents_for_unet(crop))
            kept_frames.append(frame)
            kept_coords.append([x1, y1, x2, y2])
        if not kept_frames:
            raise HTTPException(400, "no face found in avatar")

        # Пинг-понг: луп идёт вперёд и назад, и на стыке нет скачка
        frames_c = kept_frames + kept_frames[::-1]
        coords_c = kept_coords + kept_coords[::-1]
        latents_c = latents + latents[::-1]
        masks, mask_boxes = [], []
        for frame, box in zip(frames_c, coords_c):
            mask, crop_box = get_image_prepare_material(frame, box, fp=_m["fp"], mode="jaw")
            masks.append(mask)
            mask_boxes.append(crop_box)

        av = dict(frames=frames_c, coords=coords_c, latents=latents_c,
                  masks=masks, mask_boxes=mask_boxes)
        torch.save(dict(av, latents=[l.cpu() for l in latents_c]), _cache_path(avatar_id))
        _avatars[avatar_id] = av
        return av
    finally:
        shutil.rmtree(work, ignore_errors=True)


@app.get("/health")
def health():
    if not _m:
        raise HTTPException(503, "models are loading")
    return {"ok": True, "avatars_in_memory": len(_avatars)}


@app.get("/avatars/{avatar_id}")
def avatar_exists(avatar_id: str):
    _check_id(avatar_id)
    with _gpu:
        if _get_avatar(avatar_id) is None:
            raise HTTPException(404, "avatar not prepared")
    return {"ok": True}


@app.post("/avatars")
def avatar_prepare(avatar_id: str = Form(...), file: UploadFile = File(...)):
    _check_id(avatar_id)
    suffix = os.path.splitext(file.filename or "")[1] or ".mp4"
    with tempfile.NamedTemporaryFile(suffix=suffix) as f:
        shutil.copyfileobj(file.file, f)
        f.flush()
        with _gpu:
            av = _prepare(avatar_id, f.name)
    return {"ok": True, "frames": len(av["frames"])}


@torch.no_grad()
def _render(av, wav_path: str, out_path: str):
    m = _m
    feats, librosa_len = m["audio"].get_audio_feature(wav_path, weight_dtype=m["dtype"])
    chunks = m["audio"].get_whisper_chunk(
        feats, m["device"], m["dtype"], m["whisper"], librosa_len,
        fps=FPS, audio_padding_length_left=2, audio_padding_length_right=2,
    )

    h, w = av["frames"][0].shape[:2]
    side = min(h, w)
    # Квадрат по центру кадра → кружок. Готовь луп так, чтобы лицо было
    # в центре: Telegram сам обрежет углы в круг.
    vf = f"crop={side}:{side},scale={OUT_SIDE}:{OUT_SIDE}"
    ff = subprocess.Popen(
        ["ffmpeg", "-v", "error", "-y",
         "-f", "rawvideo", "-pix_fmt", "bgr24", "-s", f"{w}x{h}", "-r", str(FPS), "-i", "-",
         "-i", wav_path,
         "-vf", vf, "-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-pix_fmt", "yuv420p",
         "-c:a", "aac", "-b:a", "96k", "-shortest", "-movflags", "+faststart", out_path],
        stdin=subprocess.PIPE,
    )
    n = len(av["frames"])
    idx = 0
    try:
        for whisper_batch, latent_batch in datagen(chunks, av["latents"], BATCH):
            audio_feat = m["pe"](whisper_batch.to(m["device"]))
            latent_batch = latent_batch.to(device=m["device"], dtype=m["unet"].model.dtype)
            pred = m["unet"].model(latent_batch, m["timesteps"],
                                   encoder_hidden_states=audio_feat).sample
            pred = pred.to(device=m["device"], dtype=m["vae"].vae.dtype)
            for face in m["vae"].decode_latents(pred):
                i = idx % n
                x1, y1, x2, y2 = av["coords"][i]
                face = cv2.resize(face.astype(np.uint8), (x2 - x1, y2 - y1))
                frame = get_image_blending(av["frames"][i], face, av["coords"][i],
                                           av["masks"][i], av["mask_boxes"][i])
                ff.stdin.write(np.ascontiguousarray(frame).tobytes())
                idx += 1
    finally:
        ff.stdin.close()
        if ff.wait() != 0:
            raise HTTPException(500, "ffmpeg failed")


@app.post("/render")
def render(avatar_id: str = Form(...), audio: UploadFile = File(...)):
    _check_id(avatar_id)
    work = tempfile.mkdtemp(prefix="render-")
    try:
        raw = os.path.join(work, "in.audio")
        with open(raw, "wb") as f:
            shutil.copyfileobj(audio.file, f)
        # Режем по лимиту кружка до инференса — чтобы не считать кадры,
        # которые всё равно не влезут. Частоту не трогаем: этот же файл
        # идёт звуковой дорожкой в mp4, а whisper-фичи ресемплит сам
        wav = os.path.join(work, "speech.wav")
        subprocess.run(["ffmpeg", "-v", "error", "-y", "-i", raw, "-t", str(MAX_SECONDS),
                        "-ac", "1", wav], check=True)
        out = os.path.join(work, "circle.mp4")
        with _gpu:
            av = _get_avatar(avatar_id)
            if av is None:
                raise HTTPException(404, "avatar not prepared")
            _render(av, wav, out)
        with open(out, "rb") as f:
            return Response(f.read(), media_type="video/mp4")
    finally:
        shutil.rmtree(work, ignore_errors=True)
