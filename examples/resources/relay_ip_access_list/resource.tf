resource "relay_ip_access_list" "office" {
  workspace_id = relay_workspace.prod.id
  cidr         = "203.0.113.0/24"
  description  = "Office VPN"
}

resource "relay_ip_access_list" "ci" {
  for_each = toset(["198.51.100.7/32", "198.51.100.8/32"])

  workspace_id = relay_workspace.prod.id
  cidr         = each.value
  description  = "CI runner"
}
