package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

func clusterManagedByAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		Optional:            true,
		Computed:            true,
		Default:             stringdefault.StaticString(managedBy),
		MarkdownDescription: "Owner tag the dashboard shows. Defaults to `terraform`; set it to another tool or an empty string. Clusters created before this attribute existed are tagged on the next apply.",
	}
}

func setClusterManagedBy(ctx context.Context, c *client.Client, id string, want, current types.String, diags *diag.Diagnostics) (*serverv1.Database, bool) {
	if want.Equal(current) {
		return nil, true
	}
	out, err := c.API.UpdateDatabase(ctx, &serverv1.UpdateDatabaseRequest{Id: id, ManagedBy: want.ValueStringPointer()})
	if err != nil {
		diags.AddError("Cannot change managed_by", err.Error())
		return nil, false
	}
	return out.GetDatabase(), true
}
