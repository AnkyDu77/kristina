# ── Сборка ─────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/kristina-bot .

# ── Рантайм ────────────────────────────────────────────────────────
FROM alpine:3.21

# Не ниже версии, которой записан selectel/terraform.tfstate
ARG TF_VERSION=1.14.3
ARG TARGETARCH=amd64
# CLI Higgsfield (генерации для Instagram, higgsfield.go): Go-бинарь из
# github.com/higgsfield-ai/cli. Сумма — из checksums.json пакета
# @higgsfield/cli той же версии (linux_amd64)
ARG HF_VERSION=1.1.26
ARG HF_SHA256=5d666fae70c99b7388690191a649d1487962250ed50820e079edfd1ffac7bf5b

# openssh-client — terraform ходит по ssh в provisioner'ах main.tf;
# docker-cli — бот поднимает песочницу Кристины (computer.go)
RUN apk add --no-cache ca-certificates openssh-client tzdata curl unzip docker-cli \
 && curl -fsSL -o /tmp/tf.zip \
      "https://releases.hashicorp.com/terraform/${TF_VERSION}/terraform_${TF_VERSION}_linux_${TARGETARCH}.zip" \
 && unzip -q /tmp/tf.zip -d /usr/local/bin \
 && rm /tmp/tf.zip \
 && apk del unzip \
 && terraform version

# Кладём как higgsfield, а не hf: так его ищет бот (hf — имя CLI Hugging Face)
RUN curl -fsSL -o /tmp/hf.tgz \
      "https://github.com/higgsfield-ai/cli/releases/download/v${HF_VERSION}/hf_${HF_VERSION}_linux_${TARGETARCH}.tar.gz" \
 && echo "${HF_SHA256}  /tmp/hf.tgz" | sha256sum -c - \
 && tar -xzf /tmp/hf.tgz -C /tmp hf \
 && install -m 0755 /tmp/hf /usr/local/bin/higgsfield \
 && rm /tmp/hf.tgz /tmp/hf \
 && higgsfield version

COPY --from=build /out/kristina-bot /usr/local/bin/kristina-bot

# WORKDIR=/app/kristina-bot — чтобы относительные ../selectel, ../media и
# ../assets из .env разворачивались так же, как при локальном запуске
WORKDIR /app/kristina-bot
ENV HOME=/root
ENTRYPOINT ["kristina-bot"]
