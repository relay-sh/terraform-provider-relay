# terraform-provider-relay

Terraform and OpenTofu provider for Relay. Resource documentation lives in
[`docs/`](docs/index.md).

```hcl
provider "relay" {
  endpoint = "api.staging.userelay.sh:443"
  org_id   = "…"
}
```

Authentication uses an organization or workspace API key in `RELAY_TOKEN`.

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
`GPG_PASSPHRASE` secrets for the key registered with the Terraform and OpenTofu
registries.
