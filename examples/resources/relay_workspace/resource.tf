resource "relay_workspace" "prod" {
  name                = "prod"
  description         = "Production databases"
  deletion_protection = true
}
