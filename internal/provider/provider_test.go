package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"

	install "github.com/hashicorp/hc-install"
	"github.com/hashicorp/hc-install/checkpoint"
	"github.com/hashicorp/hc-install/product"
	"github.com/hashicorp/hc-install/src"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

const testOrgID = "8f5c2f3e-8e9b-4b4f-9d52-3f0a4f2d9a11"

func TestMain(m *testing.M) {
	cleanup := resolveTerraformCLI()
	pollInterval = time.Millisecond
	now = func() time.Time { return fakeNow }
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// Every resource.UnitTest resolves the CLI again, downloading its own copy of Terraform unless the path is pinned here.
func resolveTerraformCLI() func() {
	noop := func() {}
	if os.Getenv("TF_ACC_TERRAFORM_PATH") != "" {
		return noop
	}
	for _, candidate := range []string{"/opt/homebrew/bin/terraform", "/usr/local/bin/terraform", "/usr/bin/terraform"} {
		if _, err := os.Stat(candidate); err == nil {
			_ = os.Setenv("TF_ACC_TERRAFORM_PATH", candidate)
			return noop
		}
	}
	if path, err := exec.LookPath("terraform"); err == nil {
		_ = os.Setenv("TF_ACC_TERRAFORM_PATH", path)
		return noop
	}
	dir, err := os.MkdirTemp("", "terraform-cli")
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot create a directory for the Terraform CLI: %v\n", err)
		os.Exit(1)
	}
	path, err := install.NewInstaller().Ensure(context.Background(), []src.Source{
		&checkpoint.LatestVersion{Product: product.Terraform, InstallDir: dir},
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		fmt.Fprintf(os.Stderr, "cannot install the Terraform CLI: %v\n", err)
		os.Exit(1)
	}
	_ = os.Setenv("TF_ACC_TERRAFORM_PATH", path)
	return func() { _ = os.RemoveAll(dir) }
}

func factories(dial func(relay.Options) (*relay.Client, error)) map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"relay": func() (tfprotov6.ProviderServer, error) {
			p := &relayProvider{version: "test", dial: dial}
			return providerserver.NewProtocol6WithError(p)()
		},
	}
}

func providerBlock(f *fakeServer) string {
	return fmt.Sprintf(`
provider "relay" {
  endpoint = %q
  api_key  = "rly_key_test"
  org_id   = %q
  insecure = true
}
`, f.addr, testOrgID)
}

func seedOrg(f *fakeServer) {
	f.orgs[testOrgID] = &serverv1.Organization{Id: testOrgID, Name: "Acme", Slug: "acme"}
}

func TestProvider_EnvironmentFallback(t *testing.T) {
	f := startFakeServer(t)
	seedOrg(f)
	t.Setenv("RELAY_ENDPOINT", f.addr)
	t.Setenv("RELAY_API_KEY", "rly_key_env")
	t.Setenv("RELAY_ORG_ID", testOrgID)
	t.Setenv("RELAY_INSECURE", "true")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: `data "relay_organization" "this" {}`,
			Check:  resource.TestCheckResourceAttr("data.relay_organization.this", "slug", "acme"),
		}},
	})
	if calls := f.callsTo("GetOrganization"); len(calls) == 0 || calls[0].origin != "terraform" || calls[0].org != testOrgID {
		t.Errorf("calls = %+v", calls)
	}
}

func TestProvider_OrganizationFromAPIKey(t *testing.T) {
	f := startFakeServer(t)
	seedOrg(f)
	for _, k := range []string{"RELAY_ENDPOINT", "RELAY_API_KEY", "RELAY_ORG_ID", "RELAY_INSECURE"} {
		t.Setenv(k, "")
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
provider "relay" {
  endpoint = %q
  api_key  = "rly_key_test"
  insecure = true
}

data "relay_organization" "this" {}
`, f.addr),
			Check: resource.TestCheckResourceAttr("data.relay_organization.this", "id", testOrgID),
		}},
	})
	if calls := f.callsTo("GetOrganization"); len(calls) == 0 || calls[0].org != "" {
		t.Errorf("without org_id no org header is sent; calls = %+v", calls)
	}
}

func TestProvider_DefaultEndpoint(t *testing.T) {
	var got relay.Options
	dial := func(o relay.Options) (*relay.Client, error) {
		got = o
		return nil, errors.New("stop")
	}
	for _, k := range []string{"RELAY_ENDPOINT", "RELAY_API_KEY", "RELAY_ORG_ID", "RELAY_INSECURE"} {
		t.Setenv(k, "")
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(dial),
		Steps: []resource.TestStep{{
			Config:      `provider "relay" { api_key = "rly_key_test" }` + "\n" + `data "relay_organization" "this" {}`,
			ExpectError: regexp.MustCompile("Cannot create Relay client"),
		}},
	})
	if got.Endpoint != "" || got.Credentials != relay.APIKey("rly_key_test") || got.Insecure {
		t.Errorf("an empty endpoint is left to relay-go's default; options = %+v", got)
	}
}

func TestProvider_ConfigurationErrors(t *testing.T) {
	f := startFakeServer(t)
	cases := map[string]struct {
		config string
		env    map[string]string
		dial   func(relay.Options) (*relay.Client, error)
		want   string
	}{
		"missing api key": {
			config: fmt.Sprintf(`provider "relay" {
  endpoint = %q
  org_id   = %q
}`, f.addr, testOrgID),
			want: "Missing provider setting",
		},
		"invalid insecure env": {
			config: fmt.Sprintf(`provider "relay" {
  endpoint = %q
  api_key  = "t"
  org_id   = %q
}`, f.addr, testOrgID),
			env:  map[string]string{"RELAY_INSECURE": "maybe"},
			want: "Invalid RELAY_INSECURE",
		},
		"unknown values": {
			config: `resource "terraform_data" "late" {
  input = "x"
}
provider "relay" {
  endpoint = terraform_data.late.output
  api_key  = terraform_data.late.output
  org_id   = terraform_data.late.output
  insecure = terraform_data.late.output == "x"
}`,
			want: "Unknown provider setting",
		},
		"dial failure": {
			config: providerBlock(f),
			dial:   func(relay.Options) (*relay.Client, error) { return nil, errors.New("boom") },
			want:   "Cannot create Relay client",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			for _, k := range []string{"RELAY_ENDPOINT", "RELAY_API_KEY", "RELAY_ORG_ID", "RELAY_INSECURE"} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			dial := tc.dial
			if dial == nil {
				dial = relay.Dial
			}
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(dial),
				Steps: []resource.TestStep{{
					Config:      tc.config + "\n" + `data "relay_organization" "this" {}`,
					ExpectError: regexp.MustCompile(tc.want),
				}},
			})
		})
	}
}

func TestOrganizationDataSource(t *testing.T) {
	f := startFakeServer(t)
	seedOrg(f)
	f.orgs["other"] = &serverv1.Organization{Id: "other", Name: "Beta", Slug: "beta", Timezone: "America/Sao_Paulo", Locale: "pt-BR", SessionLengthHours: 8, CliTokenTtlHours: 24, AuditRetentionDays: 90, SessionIdleTimeoutMinutes: 30, SessionAbsoluteMaxHours: 72}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: providerBlock(f) + `
data "relay_organization" "default" {}

data "relay_organization" "by_slug" {
  slug = "beta"
}

data "relay_organization" "by_id" {
  id = "other"
}
`,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("data.relay_organization.default", "id", testOrgID),
				resource.TestCheckResourceAttr("data.relay_organization.default", "name", "Acme"),
				resource.TestCheckResourceAttr("data.relay_organization.by_slug", "id", "other"),
				resource.TestCheckResourceAttr("data.relay_organization.by_slug", "timezone", "America/Sao_Paulo"),
				resource.TestCheckResourceAttr("data.relay_organization.by_slug", "locale", "pt-BR"),
				resource.TestCheckResourceAttr("data.relay_organization.by_slug", "session_length_hours", "8"),
				resource.TestCheckResourceAttr("data.relay_organization.by_slug", "cli_token_ttl_hours", "24"),
				resource.TestCheckResourceAttr("data.relay_organization.by_slug", "audit_retention_days", "90"),
				resource.TestCheckResourceAttr("data.relay_organization.by_slug", "session_idle_timeout_minutes", "30"),
				resource.TestCheckResourceAttr("data.relay_organization.by_slug", "session_absolute_max_hours", "72"),
				resource.TestCheckResourceAttr("data.relay_organization.by_id", "slug", "beta"),
			),
		}},
	})
}

func TestOrganizationDataSource_Errors(t *testing.T) {
	cases := map[string]struct {
		config string
		prep   func(f *fakeServer)
		want   string
	}{
		"not a member": {`data "relay_organization" "x" { slug = "nope" }`, func(*fakeServer) {}, "Organization not found"},
		"api failure": {`data "relay_organization" "x" {}`, func(f *fakeServer) {
			f.failWith("GetOrganization", status.Error(codes.Unavailable, "down"))
		}, "Cannot read organization"},
		"both keys set": {`data "relay_organization" "x" {
  id   = "a"
  slug = "b"
}`, func(*fakeServer) {}, "Invalid Attribute Combination"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := startFakeServer(t)
			seedOrg(f)
			tc.prep(f)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps:                    []resource.TestStep{{Config: providerBlock(f) + tc.config, ExpectError: regexp.MustCompile(tc.want)}},
			})
		})
	}
}
