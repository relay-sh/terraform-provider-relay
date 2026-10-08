package provider

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

type ipAccessListModel struct {
	ID          types.String `tfsdk:"id"`
	WorkspaceID types.String `tfsdk:"workspace_id"`
	CIDR        types.String `tfsdk:"cidr"`
	Description types.String `tfsdk:"description"`
	CreatedBy   types.String `tfsdk:"created_by"`
	CreatedAt   types.String `tfsdk:"created_at"`
}

func ipAccessListFromProto(workspaceID types.String, e *serverv1.IPAllowlistEntry) ipAccessListModel {
	return ipAccessListModel{
		ID:          types.StringValue(e.GetId()),
		WorkspaceID: workspaceID,
		CIDR:        types.StringValue(e.GetCidr()),
		Description: types.StringValue(e.GetDescription()),
		CreatedBy:   types.StringValue(e.GetCreatedBy()),
		CreatedAt:   timestampValue(e.GetCreatedAt()),
	}
}

func ipAccessList(ctx context.Context, c *client.Client, workspaceID string) (*serverv1.ListIPAllowlistResponse, error) {
	return c.API.ListIPAllowlist(c.Workspace(ctx, workspaceID), &serverv1.ListIPAllowlistRequest{})
}

func findIPAccessListEntry(entries []*serverv1.IPAllowlistEntry, match func(*serverv1.IPAllowlistEntry) bool) *serverv1.IPAllowlistEntry {
	for _, e := range entries {
		if match(e) {
			return e
		}
	}
	return nil
}

type canonicalCIDR struct{}

func (canonicalCIDR) Description(context.Context) string {
	return "must be a CIDR in canonical form"
}

func (v canonicalCIDR) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (canonicalCIDR) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	raw := req.ConfigValue.ValueString()
	p, err := netip.ParsePrefix(raw)
	if err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid CIDR", fmt.Sprintf("Expected a network such as 203.0.113.0/24 or 203.0.113.7/32, got %q.", raw))
		return
	}
	canonical := p.Masked()
	if a := canonical.Addr(); a.Is4In6() && canonical.Bits() >= 96 {
		canonical = netip.PrefixFrom(a.Unmap(), canonical.Bits()-96).Masked()
	}
	if canonical.String() != raw {
		resp.Diagnostics.AddAttributeError(req.Path, "CIDR is not canonical", fmt.Sprintf("The API stores %q as %q. Write %q so the plan stays empty.", raw, canonical, canonical))
	}
}

type trimmed struct{}

func (trimmed) Description(context.Context) string {
	return "must not start or end with whitespace"
}

func (v trimmed) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (trimmed) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if s := req.ConfigValue.ValueString(); strings.TrimSpace(s) != s {
		resp.Diagnostics.AddAttributeError(req.Path, "Untrimmed value", "The API trims surrounding whitespace, which would leave a permanent diff.")
	}
}
