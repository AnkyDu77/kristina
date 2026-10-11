# Песочница Кристины: в ней выполняются её команды (run_command) и
# установка зависимостей (install_deps). Запускает её бот (computer.go):
# пользователь 1000, корень только на чтение, с лимитами; сеть — только у
# установки зависимостей и только с ✅ владельца. Здесь — набор инструментов.
#
# Сборка на VPS бота: docker compose --profile sandbox build sandbox
# Сеть нужна только при сборке образа: внутри песочницы её нет, поэтому
# всё, что можно поставить заранее, ставится здесь.

ARG GO_VERSION=1.27

# ── Go и его инструменты ────────────────────────────────────────────────
FROM golang:${GO_VERSION}-alpine AS gotools
RUN apk add --no-cache git \
 && go install honnef.co/go/tools/cmd/staticcheck@latest \
 && go install golang.org/x/tools/cmd/goimports@latest
# golangci-lint — готовым бинарником: собирать его из исходников на
# маленькой VPS — минуты и гигабайт памяти. Пусто — последняя версия
ARG GOLANGCI_LINT_VERSION=
RUN wget -qO- https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh \
      | sh -s -- -b /out ${GOLANGCI_LINT_VERSION}

# ── Песочница ───────────────────────────────────────────────────────────
FROM alpine:3.21

# gcc + musl-dev — для cgo (и пакетов вроде mattn/go-sqlite3);
# nodejs + npm — Node 22 LTS из Alpine, сборка фронтенда (Next.js и т.п.)
RUN apk add --no-cache bash coreutils findutils grep sed gawk file diffutils \
      python3 git jq ripgrep make curl ca-certificates tzdata \
      gcc musl-dev \
      nodejs npm \
 && adduser -D -u 1000 -h /workspace sandbox

COPY --from=gotools /usr/local/go /usr/local/go
COPY --from=gotools /go/bin/ /usr/local/bin/
COPY --from=gotools /out/golangci-lint /usr/local/bin/golangci-lint

# GOTOOLCHAIN=local: скачать другой тулчейн всё равно некуда — пусть go
# честно скажет, что версия не та, а не висит на сети
ENV PATH=/usr/local/go/bin:$PATH \
    GOTOOLCHAIN=local \
    NEXT_TELEMETRY_DISABLED=1

USER 1000:1000
WORKDIR /workspace
CMD ["sleep", "infinity"]
