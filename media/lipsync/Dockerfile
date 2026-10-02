# Тонкий слой поверх базы (base/): только код сервиса. Собирается на
# каждом подъёме GPU-машины за секунды — правка server.py не тянет за
# собой пересборку torch, mmcv и MuseTalk (20–40 минут).
ARG BASE=kristina-lipsync-base:dev
FROM ${BASE}

COPY server.py fetch_weights.sh /app/

# WORKDIR остаётся /app/MuseTalk (из базы): MuseTalk ищет веса по
# ./models, а пакет musetalk импортируется из текущего каталога
WORKDIR /app/MuseTalk
EXPOSE 9002
CMD ["python3", "-m", "uvicorn", "--app-dir", "/app", "server:app", "--host", "0.0.0.0", "--port", "9002"]
