# Тонкий слой поверх базы (base/): только код сервиса. Собирается на
# каждом подъёме GPU-машины за секунды — правка server.py не тянет за
# собой пересборку torch и зависимостей.
ARG BASE=kristina-tts-base:dev
FROM ${BASE}

WORKDIR /app
COPY server.py .

# Веса: /weights/qwen3-tts/<модель> (заранее положены в S3-кэш, см.
# tools/fetch_weights.sh). Нет их там — модель скачается с HuggingFace
# в HF_HOME при первом старте
ENV HF_HOME=/weights/hf \
    PYTHONUNBUFFERED=1

EXPOSE 9001
CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "9001"]
