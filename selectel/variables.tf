variable "vps_name" {
  description = "Имя (и hostname) GPU VPS Кристины"
  type        = string
  default     = "kristina-gpu"
}

variable "allowed_ssh_cidr" {
  description = "Список IP/диапазонов, которым разрешён SSH (нужен и IP машины с ботом — с неё terraform провижинит GPU)"
  type        = list(string)
  sensitive   = true

  validation {
    condition     = length(var.allowed_ssh_cidr) > 0
    error_message = "Нужен хотя бы один CIDR, иначе SSH закрыт всем и провижининг не пройдёт."
  }
}

variable "allowed_api_cidr" {
  description = "Кому открыт медиа-API (:8080). Нужен IP машины с ботом — она ходит в TTS и lip-sync"
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

variable "api_keys" {
  description = <<-EOT
    Bearer-ключи медиа-API (nginx отдаёт 401 без валидного
    'Authorization: Bearer <key>'). Сгенерить: openssl rand -hex 24.
    Запятых в ключах быть не должно (передаются в setup-скрипт CSV-строкой).
  EOT
  type        = list(string)
  sensitive   = true

  validation {
    condition     = length(var.api_keys) > 0
    error_message = "Нужен хотя бы один ключ, иначе медиа-API открыт всем, кто знает IP."
  }
}

variable "selectel_account_id" {
  description = "Selectel Account ID"
  type        = string
  sensitive   = true
}

variable "selectel_user_id" {
  description = "Selectel Terraform User ID"
  type        = string
  sensitive   = true
}

variable "selectel_username" {
  description = "Selectel Service User username"
  type        = string
  sensitive   = true
  default     = "terraform-admin"
}

variable "selectel_password" {
  description = "Selectel Service User password"
  type        = string
  sensitive   = true
}

variable "selectel_auth_region" {
  description = "Pool (регион) для Keystone API и Resell API endpoint"
  type        = string
  default     = "ru-7"

  validation {
    condition = contains(
      ["ru-1", "ru-2", "ru-3", "ru-7", "ru-8", "ru-9"],
      var.selectel_auth_region
    )
    error_message = "auth_region должен быть одним из: ru-1, ru-2, ru-3, ru-7, ru-8, ru-9."
  }
}

variable "vps_availability_zone" {
  description = "Зона доступности 'флейворов' vps"
  type        = string
  default     = "ru-7b"
}

variable "selectel_project_id" {
  description = "GPU Project UID"
  type        = string
  sensitive   = true
}

variable "s3_access_key" {
  description = "S3 Access key (проектный: бакет должен быть в том же проекте Selectel)"
  type        = string
  sensitive   = true
}

variable "s3_secret_key" {
  description = "S3 Secret Key"
  type        = string
  sensitive   = true
}

variable "s3_endpoint_url" {
  description = "S3 endpoint URL (пул, в котором создан бакет)"
  type        = string
  default     = "https://s3.ru-7.storage.selcloud.ru"
}

variable "s3_bucket_region" {
  description = "S3 bucket region (пул, в котором создан бакет)"
  type        = string
  default     = "ru-7"
}

variable "s3_cache_uri" {
  description = <<-EOT
    S3-префикс кэша Кристины. Внутри два каталога, оба наполняются сами
    на первом apply и дальше только читаются:
      weights/ — веса моделей (HF-кэш Qwen3-TTS + models/ MuseTalk);
      images/  — собранные docker-образы (docker save | zstd).
    Без кэша каждый подъём машины — это 20+ минут сборки образов и
    скачивания весов, и всё это время карта тикает по счёту.
  EOT
  type        = string
  default     = "s3://lmify-models/kristina-cache"
}

variable "write_cache" {
  description = <<-EOT
    Докладывать в S3 то, чего там не было (новые образы, скачанные веса).
    Выключать имеет смысл, только если кэш наполнен и его хочется
    заморозить.
  EOT
  type        = bool
  default     = true
}

variable "ready_timeout_sec" {
  description = <<-EOT
    Сколько ждать, пока TTS и lip-sync загрузят модели и ответят на
    /health. Не уложились — apply падает: машина без API бесполезна, но
    платная, и пусть об этом лучше скажет terraform, чем бот молча
    будет ловить 502.
  EOT
  type        = number
  default     = 1800
}

variable "ssh_pub_key_path" {
  description = "Path to the ssh public key"
  type        = string
  sensitive   = true
}

variable "ssh_private_key_path" {
  description = "Path to the ssh private key"
  type        = string
  sensitive   = true
}

variable "boot_disk_type" {
  description = "Selectel Disk Type"
  type        = string
}

variable "boot_disk_size_gb" {
  description = "Boot disk size, GB: два образа (~25 GB) + веса (~15 GB) + запас под кэш аватаров"
  type        = number
  default     = 120
}

variable "vps_flavor_id" {
  description = "VPS Specification ID (GPU-флейвор). Медиа-части хватает 16 GB VRAM, 24 GB — с запасом"
  type        = string
}
