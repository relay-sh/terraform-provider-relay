package provider

import (
	"context"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

type region struct {
	cloud            string
	name             string
	engines          []string
	privateEndpoints bool
}

var regionAttrTypes = map[string]attr.Type{
	"cloud":             types.StringType,
	"region":            types.StringType,
	"engines":           types.ListType{ElemType: types.StringType},
	"private_endpoints": types.BoolType,
}

func regionCatalog(ctx context.Context, c *client.Client) ([]region, error) {
	scoped := c.Org(ctx)
	databases, err := c.API.ListDatabaseProviders(scoped, &serverv1.ListDatabaseProvidersRequest{})
	if err != nil {
		return nil, err
	}
	endpoints, err := c.API.ListPrivateEndpointRegions(scoped, &serverv1.ListPrivateEndpointRegionsRequest{})
	if err != nil {
		return nil, err
	}
	byKey := map[string]*region{}
	entry := func(cloud, name string) *region {
		key := cloud + "/" + name
		if byKey[key] == nil {
			byKey[key] = &region{cloud: cloud, name: name, engines: []string{}}
		}
		return byKey[key]
	}
	for _, p := range databases.GetProviders() {
		r := entry(p.GetCloud(), p.GetRegion())
		for _, e := range p.GetEngines() {
			if e := strings.ToLower(e); !slices.Contains(r.engines, e) {
				r.engines = append(r.engines, e)
			}
		}
	}
	for _, p := range endpoints.GetRegions() {
		entry(p.GetProvider(), p.GetRegion()).privateEndpoints = true
	}
	out := make([]region, 0, len(byKey))
	for _, r := range byKey {
		slices.Sort(r.engines)
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b region) int {
		return strings.Compare(a.cloud+"/"+a.name, b.cloud+"/"+b.name)
	})
	return out, nil
}

func (r region) value() attr.Value {
	engines := make([]attr.Value, 0, len(r.engines))
	for _, e := range r.engines {
		engines = append(engines, types.StringValue(e))
	}
	return types.ObjectValueMust(regionAttrTypes, map[string]attr.Value{
		"cloud":             types.StringValue(r.cloud),
		"region":            types.StringValue(r.name),
		"engines":           types.ListValueMust(types.StringType, engines),
		"private_endpoints": types.BoolValue(r.privateEndpoints),
	})
}
