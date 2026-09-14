terraform {
  required_providers {
    relay = {
      source = "relay-sh/relay"
    }
  }
}

provider "relay" {
  endpoint = "api.staging.userelay.sh:443"
  org_id   = "00000000-0000-0000-0000-000000000000"
}
