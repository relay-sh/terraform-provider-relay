package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

var malformed = tftypes.NewValue(tftypes.String, "not an object")

func TestNew(t *testing.T) {
	p := New("1.2.3")()
	var resp provider.MetadataResponse
	p.Metadata(context.Background(), provider.MetadataRequest{}, &resp)
	if resp.TypeName != "relay" || resp.Version != "1.2.3" {
		t.Fatalf("metadata = %+v", resp)
	}
	if rp, ok := p.(*relayProvider); !ok || rp.dial == nil {
		t.Fatal("New must wire the real dialer")
	}
}

func TestMalformedConfigStopsBeforeAnyCall(t *testing.T) {
	ctx := context.Background()

	p := &relayProvider{}
	var schemaResp provider.SchemaResponse
	p.Schema(ctx, provider.SchemaRequest{}, &schemaResp)
	var configureResp provider.ConfigureResponse
	p.Configure(ctx, provider.ConfigureRequest{Config: tfsdk.Config{Schema: schemaResp.Schema, Raw: malformed}}, &configureResp)
	if !configureResp.Diagnostics.HasError() || configureResp.ResourceData != nil {
		t.Error("provider Configure must stop on a malformed config")
	}

	d := &organizationDataSource{}
	var dsSchema datasource.SchemaResponse
	d.Schema(ctx, datasource.SchemaRequest{}, &dsSchema)
	var readResp datasource.ReadResponse
	d.Read(ctx, datasource.ReadRequest{Config: tfsdk.Config{Schema: dsSchema.Schema, Raw: malformed}}, &readResp)
	if !readResp.Diagnostics.HasError() {
		t.Error("organization Read must stop on a malformed config")
	}
}
