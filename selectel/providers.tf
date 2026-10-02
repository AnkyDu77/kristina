terraform {
  required_version = ">= 1.6.0"
  required_providers {
    selectel = {
      source  = "selectel/selectel"
      version = "~> 7.0"
    }
    openstack = {
      source  = "terraform-provider-openstack/openstack"
      version = "3.4.0"
    }
  }
}

provider "selectel" {
  auth_url    = "https://cloud.api.selcloud.ru/identity/v3/"
  auth_region = var.selectel_auth_region
  domain_name = var.selectel_account_id
  username    = var.selectel_username
  password    = var.selectel_password
}

provider "openstack" {
  auth_url    = "https://cloud.api.selcloud.ru/identity/v3/"
  region      = var.selectel_auth_region
  domain_name = var.selectel_account_id
  tenant_id   = var.selectel_project_id
  user_name   = var.selectel_username
  password    = var.selectel_password
}
