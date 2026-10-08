package provider

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

const maxAPIKeyLifetime = 365 * 24 * time.Hour

func listAPIKeys(ctx context.Context, c *client.Client, workspaceID string) ([]*serverv1.APIKey, error) {
	out, err := c.API.ListAPIKeys(c.Org(ctx), &serverv1.ListAPIKeysRequest{WorkspaceId: workspaceID})
	if err != nil {
		return nil, err
	}
	return out.GetKeys(), nil
}

func apiKeyByID(keys []*serverv1.APIKey, id string) *serverv1.APIKey {
	for _, k := range keys {
		if k.GetId() == id {
			return k
		}
	}
	return nil
}

func stringSetFromGo(values []string) types.Set {
	elems := make([]attr.Value, 0, len(values))
	for _, v := range values {
		elems = append(elems, types.StringValue(v))
	}
	return types.SetValueMust(types.StringType, elems)
}

func stringSetToGo(s types.Set) []string {
	out := make([]string, 0, len(s.Elements()))
	for _, e := range s.Elements() {
		out = append(out, e.(types.String).ValueString())
	}
	return out
}

func optionalTimestamp(ts *timestamppb.Timestamp) types.String {
	if v := timestampValue(ts); v.ValueString() != "" {
		return v
	}
	return types.StringNull()
}

func parseLifetime(v types.String) (time.Duration, error) {
	d, err := time.ParseDuration(v.ValueString())
	if err != nil {
		return 0, err
	}
	if d < time.Second || d > maxAPIKeyLifetime {
		return 0, fmt.Errorf("must be between 1s and %s", maxAPIKeyLifetime)
	}
	return d, nil
}

type lifetimeValidator struct{}

func (lifetimeValidator) Description(context.Context) string {
	return "a Go duration between 1s and 8760h"
}

func (v lifetimeValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (lifetimeValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, err := parseLifetime(req.ConfigValue); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid expires_in", "Expected a duration such as 720h, at most 8760h: "+err.Error())
	}
}

type addressValidator struct{}

func (addressValidator) Description(context.Context) string {
	return "an IP address or CIDR block"
}

func (v addressValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (addressValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	value := req.ConfigValue.ValueString()
	if _, err := netip.ParsePrefix(value); err == nil {
		return
	}
	if _, err := netip.ParseAddr(value); err == nil {
		return
	}
	resp.Diagnostics.AddAttributeError(req.Path, "Invalid address", fmt.Sprintf("%q is not an IP address or CIDR block.", value))
}
