# ================================
# (5) OUTPUTS
# ================================

output "media_endpoint" {
  description = "База медиа-API Кристины (nginx → TTS /tts/, lip-sync /lipsync/). Запросы — с 'Authorization: Bearer <api_key>'. Её читает kristina-bot"
  value       = "http://${openstack_networking_floatingip_v2.floatingip.address}:${local.public_api_port}"
}

output "kristina_vps_ip" {
  description = "Public VPS IP"
  value       = openstack_networking_floatingip_v2.floatingip.address
}

output "health_check_command" {
  description = "Smoke-test с локальной машины: healthz без ключа + здоровье обоих сервисов с ключом"
  value       = "curl -s http://${openstack_networking_floatingip_v2.floatingip.address}:${local.public_api_port}/healthz && for s in tts lipsync; do curl -s -H 'Authorization: Bearer ${var.api_keys[0]}' http://${openstack_networking_floatingip_v2.floatingip.address}:${local.public_api_port}/$s/health; echo; done"
  sensitive   = true
}

output "ssh_command" {
  description = "Command to SSH into the VPS"
  value       = "ssh -i ${var.ssh_private_key_path} root@${openstack_networking_floatingip_v2.floatingip.address}"
  sensitive   = true
}

output "base_tags" {
  description = "Теги баз образов (хэши media/<svc>/base/) — по ним ищется кэш в S3"
  value       = { tts = local.tts_base_tag, lipsync = local.lipsync_base_tag }
}
