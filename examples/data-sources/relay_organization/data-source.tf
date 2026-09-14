data "relay_organization" "current" {}

data "relay_organization" "by_slug" {
  slug = "acme"
}
