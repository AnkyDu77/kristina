# База TTS: всё тяжёлое и редко меняющееся. Её тег — хэш этого каталога,
# она кэшируется в S3 и собирается заново, только если поменялись
# зависимости. Сам сервис (server.py) — тонким слоем в ../Dockerfile.
#
# Драйвер на образе Selectel — 535 (CUDA до 12.2), поэтому cu121
FROM pytorch/pytorch:2.5.1-cuda12.1-cudnn9-runtime

RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg libsndfile1 sox \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY requirements.txt constraints.txt ./
RUN pip install --no-cache-dir -r requirements.txt -c constraints.txt
