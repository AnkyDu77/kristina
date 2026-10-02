# База lip-sync: всё тяжёлое (CUDA, torch, mmcv, MuseTalk). Её тег — хэш
# этого каталога, она кэшируется в S3 и собирается заново, только если
# поменялось что-то здесь. Сервис (server.py) — тонким слоем в ../Dockerfile.
#
# Окружение — ровно как в README MuseTalk: Python 3.10, torch 2.0.1 + cu118,
# mmcv 2.0.1. Шаг влево (новый torch) — и mmcv придётся собирать из
# исходников, а это полчаса и лотерея.
FROM nvidia/cuda:11.8.0-cudnn8-runtime-ubuntu22.04

ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONUNBUFFERED=1
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      python3 python3-pip python3-dev git ffmpeg libgl1 libglib2.0-0 curl \
 && rm -rf /var/lib/apt/lists/*

RUN pip3 install --no-cache-dir -U pip \
 && pip3 install --no-cache-dir torch==2.0.1 torchvision==0.15.2 torchaudio==2.0.2 \
      --index-url https://download.pytorch.org/whl/cu118

# main клонируется в момент сборки базы, а дальше база берётся из кэша —
# то есть версия MuseTalk фактически замораживается первой сборкой.
# TODO: закрепить коммит явно, когда пайплайн заведётся
ARG MUSETALK_REF=main
RUN git clone https://github.com/TMElyralab/MuseTalk /app/MuseTalk \
 && git -C /app/MuseTalk checkout "$MUSETALK_REF"

WORKDIR /app/MuseTalk
RUN pip3 install --no-cache-dir -r requirements.txt \
 && pip3 install --no-cache-dir -U openmim \
 && mim install mmengine \
 && mim install "mmcv==2.0.1" \
 && mim install "mmdet==3.1.0"

# mmpose — без автозависимостей. Он объявляет chumpy (SMPL-модели тела),
# а тот не собирается современным pip: его setup.py 2017 года импортирует
# сам pip, которого в изолированном окружении сборки нет. В коде mmpose
# 1.1.0 chumpy не импортируется ни разу (проверено по wheel) — так что
# ставим остальные его зависимости руками. xtcocotools — версия с
# готовым wheel под cp310, чтобы ничего не компилировать.
RUN pip3 install --no-cache-dir --no-deps "mmpose==1.1.0" \
 && pip3 install --no-cache-dir json-tricks munkres "xtcocotools==1.14.3"

RUN pip3 install --no-cache-dir fastapi "uvicorn[standard]" python-multipart

# Проверка ещё на сборке: всё ли ставится и импортируется. CUDA-части
# (mmcv.ops и т.п.) тут не трогаем — при сборке нет видеокарты
RUN python3 -c "import mmengine, mmcv, mmdet, mmpose, xtcocotools.mask, json_tricks, munkres, fastapi; print('mmcv', mmcv.__version__, 'mmpose', mmpose.__version__)"
