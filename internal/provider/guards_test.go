package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
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

func assertResourceGuards(t *testing.T, r resource.Resource) {
	t.Helper()
	ctx := context.Background()
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	sch := schemaResp.Schema

	var createResp resource.CreateResponse
	r.Create(ctx, resource.CreateRequest{Plan: tfsdk.Plan{Schema: sch, Raw: malformed}}, &createResp)
	var readResp resource.ReadResponse
	r.Read(ctx, resource.ReadRequest{State: tfsdk.State{Schema: sch, Raw: malformed}}, &readResp)
	var updateResp resource.UpdateResponse
	r.Update(ctx, resource.UpdateRequest{Plan: tfsdk.Plan{Schema: sch, Raw: malformed}, State: tfsdk.State{Schema: sch, Raw: malformed}}, &updateResp)
	var deleteResp resource.DeleteResponse
	r.Delete(ctx, resource.DeleteRequest{State: tfsdk.State{Schema: sch, Raw: malformed}}, &deleteResp)

	for name, failed := range map[string]bool{
		"create": createResp.Diagnostics.HasError(),
		"read":   readResp.Diagnostics.HasError(),
		"update": updateResp.Diagnostics.HasError(),
		"delete": deleteResp.Diagnostics.HasError(),
	} {
		if !failed {
			t.Errorf("%s must stop on malformed input", name)
		}
	}
}

func TestResourceGuards(t *testing.T) {
	assertResourceGuards(t, newWorkspaceResource())

	d := newWorkspaceDataSource()
	var dsSchema datasource.SchemaResponse
	d.Schema(context.Background(), datasource.SchemaRequest{}, &dsSchema)
	var readResp datasource.ReadResponse
	d.Read(context.Background(), datasource.ReadRequest{Config: tfsdk.Config{Schema: dsSchema.Schema, Raw: malformed}}, &readResp)
	if !readResp.Diagnostics.HasError() {
		t.Error("workspace data source Read must stop on a malformed config")
	}
}
