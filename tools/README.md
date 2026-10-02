# tools/: подготовка до аренды GPU

Всё, что можно сделать без видеокарты, делается заранее: веса и
docker-образы кладутся в S3-кэш, idle-луп рендерится на маке. Тогда
GPU-машина при подъёме ничего не собирает и не качает из интернета, а
только забирает готовое из S3.

```
мак ──── fetch_weights.sh ─────► s3://lmify-models/kristina-cache/weights/
мак ──── idle-loop/make_idle.py ► assets/idle.mp4
VPS ──── build_images.sh ──────► s3://lmify-models/kristina-cache/images/  (или GPU сама, один раз)
                                     │
                     GPU VPS: aws s3 sync + docker load
```

## 0. Доступ к S3 (один раз)

```bash
cp tools/s3.env.example tools/s3.env
```

В `tools/s3.env` впиши S3-ключи из `lmify/selectel/terraform.tfvars`
(`s3_access_key` и `s3_secret_key`): бакет `lmify-models` живёт в проекте
LMIFY, а ключи у Selectel проектные. Кэш Кристины ляжет рядом с
LLM-моделями, в `s3://lmify-models/kristina-cache/`. Этот же адрес укажи
как `s3_cache_uri` в `selectel/terraform.tfvars`.

Скрипты сами обходят TLS-граблю Selectel S3 (у AWS CLI нет корня
GlobalSign R6). На маке они берут корни из системной связки ключей, на
linux — `/etc/ssl/certs`.

## (а) Веса → S3 (мак)

```bash
cd kristina
tools/fetch_weights.sh
```

- Качает ~14 GB, потом заливает в S3. Временную копию держит в
  `tools/weights-staging/`, после заливки её можно удалить: скрипт
  напечатает команду.
- Python-окружение — отдельный крошечный venv (`huggingface_hub` + `gdown`),
  без torch.
- Упал на полдороге (обычно это лимит Google Drive на face-parse) —
  просто перезапусти: скачанное повторно не качается.
- `tools/fetch_weights.sh --no-upload` — только скачать и проверить.

Что едет в кэш:

| | Размер | Откуда |
|---|---|---|
| Qwen3-TTS 1.7B Base (+ speech tokenizer) | 4.5 GB | HF Qwen |
| Qwen3-TTS 1.7B VoiceDesign | 4.5 GB | HF Qwen |
| MuseTalk 1.5 UNet | 3.4 GB | HF TMElyralab |
| sd-vae-ft-mse, whisper-tiny, DWPose | 0.9 GB | HF |
| face-parse (BiSeNet + resnet18) | 0.1 GB | Google Drive, pytorch.org |
| s3fd (детектор лица) | 0.09 GB | HF LatentSync (MuseTalk иначе качает его сам при каждом старте контейнера) |

Модели из `lmify-models/models/` Кристине не нужны: это «мозг», он живёт
в lmify.

## (б) Базы docker-образов → S3 (VPS — или сама GPU-машина)

Каждый сервис устроен как **база** (`media/<svc>/base/`: CUDA, torch,
mmcv, MuseTalk — всё тяжёлое) плюс **тонкий слой** с `server.py`. В кэш
едет только база, её тег — хэш каталога `base/`. Тонкий слой GPU-машина
собирает при каждом подъёме за секунды.

**Этот шаг не обязателен.** Не найдя базу в кэше, GPU-машина соберёт её
сама и положит в S3. Это стоит один раз 20–40 минут работы карты, дальше
база берётся из кэша. Собрать заранее на CPU-машине — просто дешевле,
плюс ошибки сборки всплывут там, а не на платной видеокарте.

Если GPU-машина собирает базу сама, первый подъём запускай руками
(`terraform apply` в терминале, а не через бота). Сборка долгая, и её
лог лучше видеть целиком.

Мак для этого не годится. Образы под CUDA собираются для amd64, а на M1
это эмуляция, часы работы и десятки гигабайт на диске, где свободно 52.
Нужна amd64-машина с Docker. Подойдёт твоя Debian 11 VPS (2 vCPU / 4 GB):
сборка — это в основном скачивание готовых пакетов, компилировать почти
нечего.

**Проверь место:** `df -h /`. Нужно ~35 GB свободных. Если их нет, временно
увеличь диск или возьми почасовую CPU-машину у Selectel: она ближе к S3,
и заливка пойдёт быстрее.

**Подготовка VPS (один раз):**

```bash
# swap: при 4 GB RAM pip на больших пакетах может упереться в память
fallocate -l 4G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile

apt update && apt install -y curl unzip zstd rsync
curl -fsSL https://get.docker.com | sh            # Docker CE, Debian 11 поддерживается
curl -sS https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip -o /tmp/awscli.zip \
  && unzip -q /tmp/awscli.zip -d /tmp && /tmp/aws/install && rm -rf /tmp/aws /tmp/awscli.zip
```

**Залить проект с мака** (весь `kristina/`: там же потом будет жить бот):

```bash
rsync -av --exclude 'tools/weights-staging' --exclude '.venv-*' --exclude 'tools/idle-loop/out' \
  ~/Documents/go/claude-workspace/kristina/ root@<IP VPS>:/root/kristina/
```

**Собрать** (под `tmux`/`screen`: lipsync собирается 20–40 минут):

```bash
cd /root/kristina && tools/build_images.sh
```

Что делает скрипт для каждого сервиса:

1. Считает тег базы по той же формуле, что терраформ (хэш
   `media/<сервис>/base/`). Если база с таким тегом уже в S3, пропускает
   сервис.
2. Собирает базу.
3. **Проверяет без GPU**: все ли модули импортируются (mmcv/mmpose/MuseTalk,
   qwen-tts) и не уехал ли torch на CUDA новее 12.2. Новее драйвер 535 на
   образе Selectel не поднимет.
4. `docker save | zstd` → S3, затем удаляет образ и кэш сборки, чтобы
   освободить место под следующий.

Правки `server.py` базу не трогают: пересобирать ничего не надо. Поменял
зависимости в `media/<сервис>/base/` — залей проект заново и запусти
скрипт ещё раз (или просто подними GPU: соберёт сама).

## (в) Живое лицо: idle-луп на маке, бесплатно

`idle-loop/make_idle.py` оживляет фото через **LivePortrait**. Чужое видео
для этого не нужно: движение генерируется. Скрипт даёт покачивания
головы на 1–2°, «дыхание», моргания и блуждание взгляда. Рот остаётся
закрытым, им потом займётся MuseTalk. Луп замыкается без шва, потому что
все движения — синусоиды с целым числом периодов.

Готовый луп уже лежит в `assets/idle.mp4` (8 с, 720×720, 25 fps, seed 7),
и бот берёт его по умолчанию. Если захочешь другой:

```bash
tools/idle-loop/setup_liveportrait.sh        # один раз: ~/Documents/LivePortrait, ~2 GB
cd tools/idle-loop
PYTORCH_ENABLE_MPS_FALLBACK=1 ~/Documents/LivePortrait/.venv/bin/python make_idle.py \
  --source ../../assets/face.png --out ../../assets/idle.mp4 --seed 11
```

Ручки: `--seed` (другой рисунок движения), `--head 0.6` (спокойнее) или
`1.5` (живее), `--blinks`, `--seconds`. Рендер на M1 идёт около 0.7 с на
кадр, 8 секунд занимают ~2.5 минуты. Бот видит новый файл по хэшу, и на
GPU аватар подготовится заново сам.

Лицензия: код LivePortrait под MIT, но детектор лица InsightFace только
для некоммерческого использования. Для личной Кристины это нормально,
для продукта — повод поменять детектор.
