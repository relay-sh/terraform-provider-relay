package provider

import (
	"context"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

var defaultConnectionRoles = []string{"readonly", "readwrite", "admin"}

var defaultConnectionsAttrTypes = map[string]attr.Type{
	"readonly":  types.BoolType,
	"readwrite": types.BoolType,
	"admin":     types.BoolType,
}

func defaultConnectionsAttribute() schema.SingleNestedAttribute {
	role := func(what string) schema.BoolAttribute {
		return schema.BoolAttribute{
			Optional: true, Computed: true,
			MarkdownDescription: "The " + what + " user and its connection. Omitted, it is `true` on create and keeps its current value afterwards.",
		}
	}
	return schema.SingleNestedAttribute{
		Optional: true, Computed: true,
		PlanModifiers: []planmodifier.Object{defaultConnectionsPlan{}},
		MarkdownDescription: "Relay-managed users the platform creates once the cluster is running, each with a Relay connection that uses it: one " +
			"read-only, one read-write and one admin. Omitted on create, all three are created, as the Relay console does; omitted afterwards, " +
			"the current value is kept. Changing a role on an existing cluster creates it, or deletes its connection and then its user, without " +
			"replacing the cluster; a user whose roles were changed outside Relay is kept. The platform applies a change after the apply returns.",
		Attributes: map[string]schema.Attribute{
			"readonly":  role("read-only"),
			"readwrite": role("read-write"),
			"admin":     role("admin"),
		},
	}
}

type defaultConnectionsPlan struct{}

func (defaultConnectionsPlan) Description(context.Context) string {
	return "Defaults every role to true on create and keeps the stored value afterwards."
}

func (m defaultConnectionsPlan) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (defaultConnectionsPlan) PlanModifyObject(ctx context.Context, req planmodifier.ObjectRequest, resp *planmodifier.ObjectResponse) {
	if req.Plan.Raw.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	creating := req.State.Raw.IsNull()
	if req.ConfigValue.IsNull() {
		switch {
		case !creating:
			resp.PlanValue = req.StateValue
		case restoring(ctx, req, resp):
			resp.PlanValue = types.ObjectUnknown(defaultConnectionsAttrTypes)
		default:
			resp.PlanValue = defaultConnectionsValue(true, true, true)
		}
		return
	}
	config, state := req.ConfigValue.Attributes(), map[string]attr.Value{}
	if known(req.StateValue) {
		state = req.StateValue.Attributes()
	}
	planned := map[string]attr.Value{}
	var removed []string
	for _, role := range defaultConnectionRoles {
		v := config[role].(types.Bool)
		prior, _ := state[role].(types.Bool)
		if v.IsNull() {
			v = types.BoolValue(true)
			if known(prior) {
				v = prior
			}
		}
		if prior.ValueBool() && known(v) && !v.ValueBool() {
			removed = append(removed, role)
		}
		planned[role] = v
	}
	resp.PlanValue = types.ObjectValueMust(defaultConnectionsAttrTypes, planned)
	if len(removed) > 0 {
		resp.Diagnostics.AddAttributeWarning(req.Path, "Default connections will be deleted",
			"Turning off "+strings.Join(removed, ", ")+" deletes those Relay connections and then their database users. "+
				"Anything still connecting with them loses access.")
	}
}

func restoring(ctx context.Context, req planmodifier.ObjectRequest, resp *planmodifier.ObjectResponse) bool {
	var restore types.Object
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("restore"), &restore)...)
	return !restore.IsNull()
}

func defaultConnectionsValue(readonly, readwrite, admin bool) types.Object {
	return types.ObjectValueMust(defaultConnectionsAttrTypes, map[string]attr.Value{
		"readonly": types.BoolValue(readonly), "readwrite": types.BoolValue(readwrite), "admin": types.BoolValue(admin),
	})
}

func defaultConnectionsToProto(o types.Object) *serverv1.DefaultConnectionsIntent {
	if !known(o) {
		return nil
	}
	attrs := o.Attributes()
	return &serverv1.DefaultConnectionsIntent{
		Readonly:  attrs["readonly"].(types.Bool).ValueBool(),
		Readwrite: attrs["readwrite"].(types.Bool).ValueBool(),
		Admin:     attrs["admin"].(types.Bool).ValueBool(),
	}
}

func defaultConnectionsFromProto(in *serverv1.DefaultConnectionsIntent, prior types.Object) types.Object {
	if in == nil {
		if prior.IsUnknown() {
			return types.ObjectNull(defaultConnectionsAttrTypes)
		}
		return prior
	}
	return defaultConnectionsValue(in.GetReadonly(), in.GetReadwrite(), in.GetAdmin())
}

func setDefaultConnections(ctx context.Context, c *client.Client, id string, planned types.Object) (*serverv1.Database, error) {
	out, err := c.API.SetDefaultConnections(ctx, &serverv1.SetDefaultConnectionsRequest{Id: id, DefaultConnections: defaultConnectionsToProto(planned)})
	return out.GetDatabase(), err
}
