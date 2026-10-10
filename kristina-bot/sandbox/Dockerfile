# Песочница Кристины: в ней выполняются её команды (run_command).
# Запускает её бот (computer.go): пользователь 1000, корень только на
# чтение, без сети, с лимитами. Здесь — только набор инструментов.
#
# Сборка на VPS бота: docker compose --profile sandbox build sandbox
FROM alpine:3.21

RUN apk add --no-cache bash coreutils findutils grep sed gawk file \
      python3 go git jq ripgrep make curl ca-certificates tzdata \
 && adduser -D -u 1000 -h /workspace sandbox

USER 1000:1000
WORKDIR /workspace
CMD ["sleep", "infinity"]
