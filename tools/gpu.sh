#!/bin/bash
# Зайти на GPU-машину и посмотреть, как прошёл её подъём. Запускать на
# VPS бота (/root/kristina/tools/gpu.sh): IP и ssh-ключ берутся из
# терраформа — те же, которыми он сам провижинит машину
# (ssh_private_key_path в selectel/terraform.tfvars).
#
#   tools/gpu.sh log [N]     — шаги установки и последние N строк лога (60)
#   tools/gpu.sh ssh [cmd]   — ssh на машину (или выполнить там cmd)
#
# Повторить упавший провижининг — /wake в боте, а не terraform apply
# руками: иначе бот не узнает, что машина поднялась, и сторож погасит её
# как сбойную.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
die() { echo "!!! $*" >&2; exit 1; }

usage() { sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 1; }
case "${1:-}" in log|ssh) ;; *) usage ;; esac

TF_DIR="${TF_DIR:-$HERE/../selectel}"
command -v terraform >/dev/null || die "нет terraform"
# ssh_command = "ssh -i <ключ> root@<ip>"; output sensitive, но -raw его отдаёт
cmd=$(terraform -chdir="$TF_DIR" output -no-color -raw ssh_command 2>/dev/null) || cmd=""
[[ $cmd =~ ^ssh\ -i\ (.+)\ root@([^ ]+)$ ]] \
  || die "в terraform state нет GPU-машины (output ssh_command пуст)"
key="${BASH_REMATCH[1]/#\~/$HOME}"
ip="${BASH_REMATCH[2]}"

# Машину пересоздают на каждый /wake, её ключ хоста каждый раз новый —
# known_hosts только мешал бы. Терраформ ключ хоста тоже не сверяет.
gpu_ssh() {
  ssh -i "$key" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR -o ConnectTimeout=10 "root@$ip" "$@"
}

case "${1:-}" in
  log)
    n="${2:-60}"
    [[ $n =~ ^[0-9]+$ ]] || die "N — число строк"
    gpu_ssh "f=/root/setup_kristina.log; [ -f \$f ] || { echo 'лога ещё нет'; exit 1; }
      grep -n '^===\|!!!' \$f | tail -25; echo -----; tail -n $n \$f"
    ;;
  ssh)
    shift
    gpu_ssh "$@"
    ;;
esac
