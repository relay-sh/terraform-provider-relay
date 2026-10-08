resource "relay_api_key" "ci" {
  name          = "ci"
  role          = "admin"
  allowed_cidrs = ["203.0.113.0/24"]
  expires_in    = "2160h"
}

resource "relay_api_key" "deployer" {
  name         = "deployer"
  workspace_id = relay_workspace.prod.id
  role         = "viewer"
}

output "ci_key" {
  value     = relay_api_key.ci.secret
  sensitive = true
}
