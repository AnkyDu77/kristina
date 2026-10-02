# ================================
# (0) LOCALS
# ================================

locals {
  # Сервисы слушают только localhost; наружу смотрит nginx, который
  # проверяет Bearer-ключ и разводит запросы по префиксу пути
  tts_port        = 9001
  lipsync_port    = 9002
  public_api_port = 8080

  root_dir    = "/opt/kristina"
  media_src   = "${path.module}/../media"
  weights_dir = "${local.root_dir}/weights"

  # Образ сервиса = тяжёлая база (media/<svc>/base/: CUDA, torch, mmcv,
  # MuseTalk) + тонкий слой с server.py. В S3-кэш едет только база, и её
  # тег — хэш одного каталога base/. Правка server.py базу не трогает:
  # тонкий слой собирается на машине за секунды. Пересборка базы
  # (20–40 минут на GPU) — только когда поменялись зависимости.
  #
  # Ту же формулу повторяет tools/lib.sh (image_tag) для сборки баз на
  # CPU-машине: менять — только синхронно. Мусор Finder'а и питона в хэш
  # не входит, иначе тег поедет от того, что каталог открыли на маке.
  media_files = [
    for f in sort(fileset(local.media_src, "**")) : f
    if !strcontains(f, ".DS_Store") && !strcontains(f, "__pycache__")
  ]
  tts_base_tag = substr(sha1(join("", [
    for f in local.media_files : filesha1("${local.media_src}/${f}") if startswith(f, "tts/base/")
  ])), 0, 12)
  lipsync_base_tag = substr(sha1(join("", [
    for f in local.media_files : filesha1("${local.media_src}/${f}") if startswith(f, "lipsync/base/")
  ])), 0, 12)
  # Любая правка в media/ (хоть одна строка server.py) перезапускает
  # провижининг — а он уже сам решит, что брать из кэша
  media_hash = sha1(join("", [for f in local.media_files : filesha1("${local.media_src}/${f}")]))
}

# ================================
# (1) GET SSH KEYS
# ================================

resource "selectel_vpc_keypair_v2" "gpu_vps_keypair" {
  name       = "kristina-keypair"
  public_key = file(pathexpand(var.ssh_pub_key_path))
  user_id    = var.selectel_user_id
}

# ================================
# (2) NETWORKING
# ================================

resource "openstack_networking_network_v2" "priv_network" {
  name           = "kristina-private-network"
  admin_state_up = true
}

resource "openstack_networking_subnet_v2" "priv_subnet" {
  name       = "kristina-private-subnet"
  network_id = openstack_networking_network_v2.priv_network.id
  cidr       = "192.168.199.0/24"
}

data "openstack_networking_network_v2" "external_net" {
  external = true
}

resource "openstack_networking_router_v2" "net_router" {
  name                = "kristina-router"
  external_network_id = data.openstack_networking_network_v2.external_net.id
}

resource "openstack_networking_router_interface_v2" "net_router_interface" {
  router_id = openstack_networking_router_v2.net_router.id
  subnet_id = openstack_networking_subnet_v2.priv_subnet.id
}

resource "openstack_networking_floatingip_v2" "floatingip" {
  pool       = "external-network"
  depends_on = [openstack_networking_router_interface_v2.net_router_interface]
}

resource "openstack_networking_port_v2" "net_port" {
  name       = "kristina-port"
  network_id = openstack_networking_network_v2.priv_network.id
  fixed_ip {
    subnet_id = openstack_networking_subnet_v2.priv_subnet.id
  }
  admin_state_up = true
  # SG вешаем на порт, а не на инстанс: при подключении через port
  # provider не видит SG инстанса и на каждом apply пытается добавить её
  # повторно → Neutron 400 "Duplicate items"
  security_group_ids = [openstack_networking_secgroup_v2.kristina_sg.id]
}

resource "openstack_networking_secgroup_v2" "kristina_sg" {
  name        = "kristina-security-group"
  description = "Security group for Kristina media GPU VPS"
}

// По правилу на каждый CIDR: remote_ip_prefix принимает только один.
// nonsensitive() нужен, потому что count не работает с sensitive-значением.
resource "openstack_networking_secgroup_rule_v2" "ssh" {
  count             = length(nonsensitive(var.allowed_ssh_cidr))
  security_group_id = openstack_networking_secgroup_v2.kristina_sg.id
  direction         = "ingress"
  ethertype         = "IPv4"
  protocol          = "tcp"
  port_range_max    = 22
  port_range_min    = 22
  remote_ip_prefix  = var.allowed_ssh_cidr[count.index]
}

resource "openstack_networking_secgroup_rule_v2" "media_api" {
  count             = length(var.allowed_api_cidr)
  security_group_id = openstack_networking_secgroup_v2.kristina_sg.id
  direction         = "ingress"
  ethertype         = "IPv4"
  protocol          = "tcp"
  port_range_max    = local.public_api_port
  port_range_min    = local.public_api_port
  remote_ip_prefix  = var.allowed_api_cidr[count.index]
}

# ================================
# (3) CREATING VPS
# ================================

data "openstack_images_image_v2" "gpu_vps_img" {
  name        = "Ubuntu 24.04 LTS 64-bit GPU Driver 535"
  most_recent = true
  visibility  = "public"
}

resource "openstack_blockstorage_volume_v3" "disk" {
  name                 = "kristina-boot-volume"
  size                 = var.boot_disk_size_gb
  image_id             = data.openstack_images_image_v2.gpu_vps_img.id
  volume_type          = var.boot_disk_type
  availability_zone    = var.vps_availability_zone
  enable_online_resize = true

  lifecycle {
    ignore_changes = [image_id]
  }
}

resource "openstack_compute_instance_v2" "kristina_vps" {
  name                    = var.vps_name
  flavor_id               = var.vps_flavor_id
  key_pair                = selectel_vpc_keypair_v2.gpu_vps_keypair.name
  availability_zone_hints = var.vps_availability_zone

  network {
    port = openstack_networking_port_v2.net_port.id
  }

  block_device {
    uuid                  = openstack_blockstorage_volume_v3.disk.id
    source_type           = "volume"
    destination_type      = "volume"
    boot_index            = 0
    delete_on_termination = true
  }

  vendor_options {
    ignore_resize_confirmation = true
  }

  tags = ["preemptible", "--os-compute-api-version 2.72"]
}

resource "openstack_networking_floatingip_associate_v2" "ip_association" {
  port_id     = openstack_networking_port_v2.net_port.id
  floating_ip = openstack_networking_floatingip_v2.floatingip.address
  depends_on  = [openstack_compute_instance_v2.kristina_vps]
}

# ================================
# (4) PROVISIONING: docker + TTS + lip-sync + nginx-гейт
# ================================

# Секреты передаются аргументами $1..$3, чтобы не светить их в файле
# скрипта (он лежит рядом с main.tf и в .gitignore).
resource "local_file" "setup_script" {
  filename = "${path.module}/setup_kristina.sh"
  content  = <<-EOT
  #!/bin/bash
  set -euo pipefail

  AWS_ACCESS_KEY_ID="$1"
  AWS_SECRET_ACCESS_KEY="$2"
  API_KEYS="$3" # CSV
  export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY

  # AWS CLI v2 таскает свой CA-bundle без корня GlobalSign R6, которым подписан
  # серт Selectel S3 — используем системное хранилище Ubuntu
  export AWS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt
  S3="--endpoint-url ${var.s3_endpoint_url} --region ${var.s3_bucket_region}"
  # --only-show-errors понимают только cp/sync/mv/rm; `aws s3 ls` с ним
  # падает на «Unknown options» — и проверка «есть ли образ в кэше» молча
  # отвечала бы «нет», а GPU-машина пересобирала бы образы каждый раз
  Q="--only-show-errors"
  CACHE="${var.s3_cache_uri}"
  WRITE_CACHE="${var.write_cache}"

  export DEBIAN_FRONTEND=noninteractive
  APT="apt-get -o DPkg::Lock::Timeout=600 -y"

  echo "=== Installing system packages ==="
  $APT update
  $APT install curl unzip jq nginx zstd ca-certificates gnupg docker.io docker-compose-v2 docker-buildx

  echo "=== Installing AWS CLI ==="
  curl -sS "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o /root/awscliv2.zip
  unzip -q -o /root/awscliv2.zip -d /root
  /root/aws/install --update
  rm -rf /root/aws /root/awscliv2.zip

  echo "=== Installing NVIDIA Container Toolkit ==="
  # Драйвер уже в образе Selectel; контейнерам нужен только мост к нему
  curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
    | gpg --dearmor --yes -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
  curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
    | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
    > /etc/apt/sources.list.d/nvidia-container-toolkit.list
  $APT update
  $APT install nvidia-container-toolkit
  nvidia-ctk runtime configure --runtime=docker
  systemctl restart docker
  nvidia-smi --query-gpu=name,memory.total --format=csv

  if ! aws s3 ls "$CACHE/" $S3 > /dev/null 2>&1; then
    # Пустой префикс S3 тоже отвечает ошибкой — поэтому не падаем, а
    # предупреждаем: на первом apply кэша ещё нет, и это нормально
    echo "!!! $CACHE пуст или недоступен этими ключами. Первый подъём — норма;"
    echo "!!! иначе проверь, что бакет в пуле ${var.s3_bucket_region} и ключи от НУЖНОГО проекта Selectel"
  fi

  echo "=== Weights from S3 cache ==="
  mkdir -p ${local.weights_dir}/hf ${local.weights_dir}/qwen3-tts ${local.weights_dir}/musetalk ${local.root_dir}/cache
  aws s3 sync "$CACHE/weights" ${local.weights_dir} $S3 $Q || echo "кэша весов нет — скачаем с HuggingFace"

  cd ${local.root_dir}/media
  cat > .env <<ENV
  KRISTINA_TTS_BASE=kristina-tts-base:${local.tts_base_tag}
  KRISTINA_LIPSYNC_BASE=kristina-lipsync-base:${local.lipsync_base_tag}
  KRISTINA_ROOT=${local.root_dir}
  ENV

  # База из кэша грузится за пару минут; сборка с нуля (torch, mmcv,
  # клон MuseTalk) — двадцать-сорок, и её хочется платить один раз. Если
  # базы в кэше нет, её собирает сама GPU-машина и кладёт в S3 — так что
  # заранее собирать на CPU-машине (tools/build_images.sh) не обязательно,
  # это лишь экономит деньги за первую сборку.
  load_or_build_base() {
    local svc="$1" tag="$2"
    local img="kristina-$svc-base:$tag"
    local key="$CACHE/images/kristina-$svc-base-$tag.tar.zst"
    if aws s3 ls "$key" $S3 > /dev/null 2>&1; then
      echo "=== $img — из кэша ==="
      aws s3 cp "$key" - $S3 $Q | zstd -d | docker load
    else
      echo "=== $img — собираем (в кэше нет; lipsync — 20–40 минут) ==="
      docker build -t "$img" "$svc/base"
      if [ "$WRITE_CACHE" = "true" ]; then
        docker save "$img" | zstd -T0 -3 | aws s3 cp - "$key" $S3 $Q \
          || echo "!!! база $svc не уехала в кэш — в следующий раз соберётся заново"
      fi
    fi
  }
  load_or_build_base tts ${local.tts_base_tag}
  load_or_build_base lipsync ${local.lipsync_base_tag}

  echo "=== Тонкие образы сервисов поверх баз (секунды) ==="
  docker compose build

  if [ ! -f ${local.weights_dir}/musetalk/musetalkV15/unet.pth ]; then
    echo "=== MuseTalk weights — качаем ==="
    docker compose run --rm --no-deps lipsync bash /app/fetch_weights.sh
  fi

  echo "=== Starting services ==="
  docker compose up -d

  echo "=== Configuring nginx API gateway ==="
  : > /etc/nginx/kristina_api_keys.conf
  chmod 600 /etc/nginx/kristina_api_keys.conf
  IFS=',' read -ra KEYS <<< "$API_KEYS"
  for k in "$${KEYS[@]}"; do
    printf '"Bearer %s" 1;\n' "$k" >> /etc/nginx/kristina_api_keys.conf
  done

  cat > /etc/nginx/sites-available/kristina <<'NGINX'
  # Bearer-ключи длиннее 64 байт не влезают в дефолтный map-бакет
  map_hash_bucket_size 128;

  map $http_authorization $kristina_auth_ok {
      default 0;
      include /etc/nginx/kristina_api_keys.conf;
  }

  server {
      listen ${local.public_api_port};
      # эталон голоса и луп аватара едут в теле запроса
      client_max_body_size 100m;
      # подготовка аватара из лупа — минуты, рендер кружка — десятки секунд
      proxy_read_timeout 900s;
      proxy_send_timeout 900s;

      location = /healthz {
          return 200 'ok';
      }

      location /tts/ {
          if ($kristina_auth_ok = 0) {
              return 401 '{"error":"invalid or missing api key"}';
          }
          proxy_pass http://127.0.0.1:${local.tts_port}/;
      }

      location /lipsync/ {
          if ($kristina_auth_ok = 0) {
              return 401 '{"error":"invalid or missing api key"}';
          }
          proxy_pass http://127.0.0.1:${local.lipsync_port}/;
      }
  }
  NGINX

  ln -sf /etc/nginx/sites-available/kristina /etc/nginx/sites-enabled/kristina
  rm -f /etc/nginx/sites-enabled/default
  nginx -t
  systemctl enable --now nginx
  systemctl reload nginx

  # Готовность — это ответивший /health, а не поднятый контейнер: модели
  # грузятся (а на первом подъёме ещё и качаются) уже после старта
  echo "=== Waiting for models (up to ${var.ready_timeout_sec}s) ==="
  DEADLINE=$(( $(date +%s) + ${var.ready_timeout_sec} ))
  for port in ${local.tts_port} ${local.lipsync_port}; do
    until curl -fsS -m 5 "http://127.0.0.1:$port/health" > /dev/null 2>&1; do
      if [ "$(date +%s)" -gt "$DEADLINE" ]; then
        echo "!!! сервис на :$port не ответил за ${var.ready_timeout_sec}s"
        docker compose ps
        docker compose logs --tail 80
        exit 1
      fi
      sleep 10
    done
    echo "сервис на :$port готов"
  done
  nvidia-smi --query-gpu=memory.total,memory.used --format=csv

  if [ "$WRITE_CACHE" = "true" ]; then
    echo "=== Weights → S3 cache ==="
    aws s3 sync ${local.weights_dir} "$CACHE/weights" $S3 $Q \
      || echo "!!! веса не уехали в кэш — в следующий раз скачаются заново"
  fi

  echo "=== Setup complete. Watch: cd ${local.root_dir}/media && docker compose logs -f ==="
  EOT
}

resource "null_resource" "provision" {
  depends_on = [
    openstack_networking_floatingip_associate_v2.ip_association,
    local_file.setup_script,
  ]

  # Перезапускаем провижининг при изменении скрипта, исходников сервисов,
  # аргументов (секреты — хэшем, чтобы не класть их в state открытым
  # текстом) или пересоздании VPS
  triggers = {
    instance_id = openstack_compute_instance_v2.kristina_vps.id
    setup_hash  = local_file.setup_script.content_sha1
    media_hash  = local.media_hash
    args_hash = sha256(join("|", [
      var.s3_access_key,
      var.s3_secret_key,
      join(",", var.api_keys),
    ]))
  }

  connection {
    type        = "ssh"
    user        = "root"
    private_key = file(pathexpand(var.ssh_private_key_path))
    host        = openstack_networking_floatingip_v2.floatingip.address
    timeout     = "5m" # SSH-ретраи заменяют фиксированный time_sleep
  }

  provisioner "remote-exec" {
    inline = ["mkdir -p ${local.root_dir}/media"]
  }

  # Слэш в конце source — копируем содержимое каталога, а не сам каталог
  provisioner "file" {
    source      = "${local.media_src}/"
    destination = "${local.root_dir}/media"
  }

  provisioner "file" {
    source      = local_file.setup_script.filename
    destination = "/root/setup_kristina.sh"
  }

  provisioner "remote-exec" {
    inline = [
      "chmod +x /root/setup_kristina.sh",
      "/root/setup_kristina.sh '${var.s3_access_key}' '${var.s3_secret_key}' '${join(",", var.api_keys)}'"
    ]
  }
}
