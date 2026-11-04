resource "relay_access_grant" "payments" {
  workspace_id = relay_workspace.prod.id
  grantee_type = "team"
  grantee      = "payments"
  target_type  = "connection"
  target       = relay_connection.payments_app.name
}

resource "relay_access_grant" "oncall" {
  workspace_id = relay_workspace.prod.id
  grantee_type = "user"
  grantee      = "alex@example.com"
  target_type  = "connection_group"
  target       = "production-readonly"
  expires_at   = "2026-12-01T18:00:00Z"
}
