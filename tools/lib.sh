# Общее для скриптов tools/: настройки S3 и тег образа.
# Подключается через source, сам по себе не запускается.

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

die() { echo "!!! $*" >&2; exit 1; }

# Настройки — из tools/s3.env (рядом, в .gitignore) или из окружения
[ -f "$HERE/s3.env" ] && set -a && . "$HERE/s3.env" && set +a

S3_ENDPOINT="${S3_ENDPOINT:-https://s3.ru-7.storage.selcloud.ru}"
S3_REGION="${S3_REGION:-ru-7}"

s3_init() {
  command -v aws >/dev/null || die "нет aws CLI (mac: brew install awscli; debian: см. tools/README.md)"
  [ -n "${KRISTINA_S3_CACHE:-}" ] || die "KRISTINA_S3_CACHE не задан (tools/s3.env)"
  [ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ] \
    || die "AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY не заданы (tools/s3.env)"
  export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY

  # Серт Selectel S3 подписан GlobalSign R6, которого нет в CA-bundle
  # самого AWS CLI (грабля из lmify). Даём ему системное хранилище.
  if [ -z "${AWS_CA_BUNDLE:-}" ]; then
    if [ "$(uname -s)" = Darwin ]; then
      AWS_CA_BUNDLE="$(mktemp -t kristina-ca).pem"
      security find-certificate -a -p /System/Library/Keychains/SystemRootCertificates.keychain > "$AWS_CA_BUNDLE"
    else
      AWS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt
    fi
    export AWS_CA_BUNDLE
  fi

  # Доступ проверяем на корне бакета: несуществующий ещё префикс кэша
  # ls считает ошибкой, а это нормально до первой заливки. Ошибку aws
  # показываем как есть — гадать о причине за него не надо
  local bucket err
  bucket="$(echo "$KRISTINA_S3_CACHE" | cut -d/ -f1-3)"
  if ! err="$(s3 s3 ls "$bucket/" 2>&1 >/dev/null)"; then
    echo "$err" >&2
    die "нет доступа к $bucket (InvalidAccessKeyId/AccessDenied — ключи не те или от другого проекта Selectel)"
  fi
}

# --only-show-errors понимают только cp/sync/mv/rm: `aws s3 ls` с ним
# падает на «Unknown options»
s3() {
  local q=""
  case "${1:-} ${2:-}" in
    "s3 cp" | "s3 sync" | "s3 mv" | "s3 rm") q="--only-show-errors" ;;
  esac
  aws "$@" --endpoint-url "$S3_ENDPOINT" --region "$S3_REGION" $q
}

_sha1() { if command -v sha1sum >/dev/null; then sha1sum; else shasum -a 1; fi; }

# image_tag <media/<svc>/base> — ТА ЖЕ формула, что tts_base_tag и
# lipsync_base_tag в selectel/main.tf: sha1 от склейки sha1 всех файлов в порядке сортировки
# путей, первые 12 символов. Разойдутся — GPU-машина не найдёт образ в
# кэше и начнёт собирать его сама (долго, но не смертельно).
image_tag() {
  (cd "$1" && find . -type f ! -name .DS_Store ! -path '*__pycache__*' \
    | sed 's|^\./||' | LC_ALL=C sort \
    | while IFS= read -r f; do _sha1 < "$f" | cut -c1-40; done \
    | tr -d '\n' | _sha1 | cut -c1-12)
}
