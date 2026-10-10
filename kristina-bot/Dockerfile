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

# openssh-client — terraform ходит по ssh в provisioner'ах main.tf;
# docker-cli — бот поднимает песочницу Кристины (computer.go)
RUN apk add --no-cache ca-certificates openssh-client tzdata curl unzip docker-cli \
 && curl -fsSL -o /tmp/tf.zip \
      "https://releases.hashicorp.com/terraform/${TF_VERSION}/terraform_${TF_VERSION}_linux_${TARGETARCH}.zip" \
 && unzip -q /tmp/tf.zip -d /usr/local/bin \
 && rm /tmp/tf.zip \
 && apk del unzip \
 && terraform version

COPY --from=build /out/kristina-bot /usr/local/bin/kristina-bot

# WORKDIR=/app/kristina-bot — чтобы относительные ../selectel, ../media и
# ../assets из .env разворачивались так же, как при локальном запуске
WORKDIR /app/kristina-bot
ENV HOME=/root
ENTRYPOINT ["kristina-bot"]
