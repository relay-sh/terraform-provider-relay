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

var (
	defaultConnectionRoles           = []string{"readonly", "readwrite", "admin"}
	documentDBDefaultConnectionRoles = []string{"readonly", "admin"}
	defaultConnectionsAttrTypes      = defaultConnectionsTypes(defaultConnectionRoles)
	defaultConnectionRoleNames       = map[string]string{"readonly": "read-only", "readwrite": "read-write", "admin": "admin"}
)

func defaultConnectionsTypes(roles []string) map[string]attr.Type {
	out := make(map[string]attr.Type, len(roles))
	for _, role := range roles {
		out[role] = types.BoolType
	}
	return out
}

func defaultConnectionsAttribute() schema.SingleNestedAttribute {
	return defaultConnectionsAttributeFor(defaultConnectionsPlan{roles: defaultConnectionRoles, restorable: true},
		"one read-only, one read-write and one admin. Omitted on create, all three are created")
}

func documentDBDefaultConnectionsAttribute() schema.SingleNestedAttribute {
	return defaultConnectionsAttributeFor(defaultConnectionsPlan{roles: documentDBDefaultConnectionRoles},
		"one read-only and one admin, since DocumentDB grants writes only with administration. Omitted on create, both are created")
}

func defaultConnectionsAttributeFor(m defaultConnectionsPlan, users string) schema.SingleNestedAttribute {
	attrs := make(map[string]schema.Attribute, len(m.roles))
	for _, role := range m.roles {
		attrs[role] = schema.BoolAttribute{
			Optional: true, Computed: true,
			MarkdownDescription: "The " + defaultConnectionRoleNames[role] + " user and its connection. Omitted, it is `true` on create and keeps its current value afterwards.",
		}
	}
	return schema.SingleNestedAttribute{
		Optional: true, Computed: true,
		PlanModifiers: []planmodifier.Object{m},
		MarkdownDescription: "Relay-managed users the platform creates once the cluster is running, each with a Relay connection that uses it: " +
			users + ", as the Relay console does; omitted afterwards, " +
			"the current value is kept. Changing a role on an existing cluster creates it, or deletes its connection and then its user, without " +
			"replacing the cluster; a user whose roles were changed outside Relay is kept. The platform applies a change after the apply returns.",
		Attributes: attrs,
	}
}

type defaultConnectionsPlan struct {
	roles      []string
	restorable bool
}

func (defaultConnectionsPlan) Description(context.Context) string {
	return "Defaults every role to true on create and keeps the stored value afterwards."
}

func (m defaultConnectionsPlan) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (m defaultConnectionsPlan) PlanModifyObject(ctx context.Context, req planmodifier.ObjectRequest, resp *planmodifier.ObjectResponse) {
	if req.Plan.Raw.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	creating := req.State.Raw.IsNull()
	if req.ConfigValue.IsNull() {
		switch {
		case !creating:
			resp.PlanValue = req.StateValue
		case m.restorable && restoring(ctx, req, resp):
			resp.PlanValue = types.ObjectUnknown(defaultConnectionsTypes(m.roles))
		default:
			resp.PlanValue = m.allOn()
		}
		return
	}
	config, state := req.ConfigValue.Attributes(), map[string]attr.Value{}
	if known(req.StateValue) {
		state = req.StateValue.Attributes()
	}
	planned := map[string]attr.Value{}
	var removed []string
	for _, role := range m.roles {
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
	resp.PlanValue = types.ObjectValueMust(defaultConnectionsTypes(m.roles), planned)
	if len(removed) > 0 {
		resp.Diagnostics.AddAttributeWarning(req.Path, "Default connections will be deleted",
			"Turning off "+strings.Join(removed, ", ")+" deletes those Relay connections and then their database users. "+
				"Anything still connecting with them loses access.")
	}
}

func (m defaultConnectionsPlan) allOn() types.Object {
	values := make(map[string]attr.Value, len(m.roles))
	for _, role := range m.roles {
		values[role] = types.BoolValue(true)
	}
	return types.ObjectValueMust(defaultConnectionsTypes(m.roles), values)
}

func restoring(ctx context.Context, req planmodifier.ObjectRequest, resp *planmodifier.ObjectResponse) bool {
	var restore types.Object
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("restore"), &restore)...)
	return !restore.IsNull()
}

func defaultConnectionsToProto(o types.Object) *serverv1.DefaultConnectionsIntent {
	if !known(o) {
		return nil
	}
	attrs := o.Attributes()
	on := func(role string) bool {
		v, _ := attrs[role].(types.Bool)
		return v.ValueBool()
	}
	return &serverv1.DefaultConnectionsIntent{Readonly: on("readonly"), Readwrite: on("readwrite"), Admin: on("admin")}
}

func defaultConnectionsFromProto(in *serverv1.DefaultConnectionsIntent, prior types.Object) types.Object {
	attrTypes := prior.AttributeTypes(context.Background())
	if in == nil {
		if prior.IsUnknown() {
			return types.ObjectNull(attrTypes)
		}
		return prior
	}
	values := map[string]bool{"readonly": in.GetReadonly(), "readwrite": in.GetReadwrite(), "admin": in.GetAdmin()}
	elems := make(map[string]attr.Value, len(attrTypes))
	for role := range attrTypes {
		elems[role] = types.BoolValue(values[role])
	}
	return types.ObjectValueMust(attrTypes, elems)
}

func setDefaultConnections(ctx context.Context, c *client.Client, id string, planned types.Object) (*serverv1.Database, error) {
	out, err := c.API.SetDefaultConnections(ctx, &serverv1.SetDefaultConnectionsRequest{Id: id, DefaultConnections: defaultConnectionsToProto(planned)})
	return out.GetDatabase(), err
}
