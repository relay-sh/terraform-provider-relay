terraform {
  required_providers {
    relay = {
      source = "relay-sh/relay"
    }
  }
}

variable "relay_api_key" {
  type      = string
  sensitive = true
}

provider "relay" {
  api_key = var.relay_api_key
}
