# terraform-provider-relay

Terraform and OpenTofu provider for Relay. Resource documentation lives in
[`docs/`](docs/index.md).

```hcl
provider "relay" {
  api_key = var.relay_api_key
}
```

Authentication uses an organization or workspace API key, from `api_key` or
`RELAY_API_KEY`. The organization comes from the key.

### OpenTofu

The provider is published on the Terraform registry. Until it is listed on the
OpenTofu registry ([#88](https://github.com/relay-sh/terraform-provider-relay/issues/88)),
`tofu init` cannot resolve `relay-sh/relay`; spell the source with the
Terraform registry host instead:

```hcl
terraform {
  required_providers {
    relay = {
      source = "registry.terraform.io/relay-sh/relay"
    }
  }
}
```

Tests against staging set `RELAY_ENDPOINT=api.staging.userelay.sh:443`; it
defaults to production.

## Development

`relay-go` is private, so dependencies are vendored. After changing `go.mod`:

```sh
GOPRIVATE='github.com/relay-sh/*' go mod tidy
GOPRIVATE='github.com/relay-sh/*' go mod vendor
```

Tests run the real Terraform CLI against an in-process fake Relay API. They pick
up `terraform` from `TF_ACC_TERRAFORM_PATH`, the usual install paths, or
download it:

```sh
go test ./internal/...
```

Regenerate `docs/` after a schema change:

```sh
go generate ./...
```

## Releasing

Pushing a `v*` tag runs GoReleaser. The repository needs `GPG_PRIVATE_KEY` and
`GPG_PASSPHRASE` secrets for the signing key. Its public half is
[`signing-key.asc`](signing-key.asc) (RSA, no expiry, fingerprint
`BB05E093C2E7572CE7BCD9A9E6C978778A127EAB`, key ID `E6C978778A127EAB`),
registered with the Terraform registry. Each release carries
`terraform-provider-relay_<version>_SHA256SUMS`, its detached `.sig`, the
`_manifest.json` (protocol 6) and one zip per platform, which is the layout the
OpenTofu registry reads too. Rotating the key means registering the new public
key with both registries before the next tag.
