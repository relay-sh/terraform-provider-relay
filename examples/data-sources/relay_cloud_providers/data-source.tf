data "relay_cloud_providers" "all" {}

output "served" {
  value = { for c in data.relay_cloud_providers.all.cloud_providers : c.name => c.regions }
}
