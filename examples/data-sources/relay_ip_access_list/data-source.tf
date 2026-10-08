data "relay_ip_access_list" "office" {
  workspace_id = data.relay_workspace.prod.id
  cidr         = "203.0.113.0/24"
}
