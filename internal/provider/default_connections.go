package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

func defaultConnectionsAttribute(userResource string) schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		Optional:      true,
		PlanModifiers: []planmodifier.Object{defaultConnectionsPlan{}},
		MarkdownDescription: "Relay-managed users the platform creates once the cluster is running, each with a Relay connection that uses it: one " +
			"read-only, one read-write and one admin. Omit it for all three, as the Relay console does; set a role to `false` to skip it. Only read on " +
			"create: the platform cannot add them to a cluster that already exists, so setting or changing it there only stores the value, without " +
			"replacing the cluster. For such a cluster, declare the users with `" + userResource + "` and their connections with `relay_connection`. " +
			"Not available with `restore`.",
		Attributes: map[string]schema.Attribute{
			"readonly":  schema.BoolAttribute{Required: true, MarkdownDescription: "Create the read-only user and connection."},
			"readwrite": schema.BoolAttribute{Required: true, MarkdownDescription: "Create the read-write user and connection."},
			"admin":     schema.BoolAttribute{Required: true, MarkdownDescription: "Create the admin user and connection."},
		},
	}
}

type defaultConnectionsPlan struct{}

func (defaultConnectionsPlan) Description(context.Context) string {
	return "Rejects it with restore and warns that it does not reach an existing cluster."
}

func (m defaultConnectionsPlan) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (defaultConnectionsPlan) PlanModifyObject(ctx context.Context, req planmodifier.ObjectRequest, resp *planmodifier.ObjectResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	if req.State.Raw.IsNull() {
		var restore types.Object
		resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("restore"), &restore)...)
		if !restore.IsNull() && !req.ConfigValue.IsNull() {
			resp.Diagnostics.AddAttributeError(req.Path, "Not available with restore",
				"A restored cluster starts from the source's backup, and the platform does not create default connections for it. Remove default_connections.")
		}
		return
	}
	if !req.ConfigValue.Equal(req.StateValue) {
		resp.Diagnostics.AddAttributeWarning(req.Path, "Default connections are only created with the cluster",
			"This cluster already exists, so the platform will not add or remove default connections: the new value is only stored. "+
				"To give it connections, declare the users and relay_connection resources for them.")
	}
}

func defaultConnectionsToProto(o types.Object) *serverv1.DefaultConnectionsIntent {
	if o.IsNull() {
		return &serverv1.DefaultConnectionsIntent{Readonly: true, Readwrite: true, Admin: true}
	}
	attrs := o.Attributes()
	return &serverv1.DefaultConnectionsIntent{
		Readonly:  attrs["readonly"].(types.Bool).ValueBool(),
		Readwrite: attrs["readwrite"].(types.Bool).ValueBool(),
		Admin:     attrs["admin"].(types.Bool).ValueBool(),
	}
}
