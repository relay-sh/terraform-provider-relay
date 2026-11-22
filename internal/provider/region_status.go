package provider

import (
	"cmp"
	"slices"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	datasourceschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

var regionStatusType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"cloud":            types.StringType,
	"region":           types.StringType,
	"role":             types.StringType,
	"phase":            types.StringType,
	"ready_nodes":      types.Int64Type,
	"total_nodes":      types.Int64Type,
	"replication_link": types.StringType,
	"held":             types.StringType,
	"message":          types.StringType,
}}

const regionsDescription = "Per-region status of a standby distribution, one entry per region, empty for a single-region cluster. " +
	"It is read from the platform and changes by itself, for example when a standby's replication link drops, so it never shows as drift."

var regionStatusDescriptions = map[string]string{
	"cloud":            "Cloud of the region.",
	"region":           "Region name.",
	"role":             "`primary` or `standby`.",
	"phase":            "Lifecycle phase of the cluster in this region.",
	"ready_nodes":      "Nodes ready in this region.",
	"total_nodes":      "Nodes expected in this region.",
	"replication_link": "`Up` or `Down` on a standby, as the operator reports it; empty on the primary.",
	"held":             "Why the operator holds this region's role change; empty when nothing is held.",
	"message":          "Detail for the phase.",
}

func regionsAttribute() schema.Attribute {
	attrs := map[string]schema.Attribute{}
	for name, description := range regionStatusDescriptions {
		if t := regionStatusType.AttrTypes[name]; t == types.Int64Type {
			attrs[name] = schema.Int64Attribute{Computed: true, MarkdownDescription: description}
		} else {
			attrs[name] = schema.StringAttribute{Computed: true, MarkdownDescription: description}
		}
	}
	return schema.SetNestedAttribute{
		Computed:            true,
		NestedObject:        schema.NestedAttributeObject{Attributes: attrs},
		MarkdownDescription: regionsDescription,
	}
}

func regionsDataAttribute() datasourceschema.Attribute {
	attrs := map[string]datasourceschema.Attribute{}
	for name, description := range regionStatusDescriptions {
		if t := regionStatusType.AttrTypes[name]; t == types.Int64Type {
			attrs[name] = datasourceschema.Int64Attribute{Computed: true, MarkdownDescription: description}
		} else {
			attrs[name] = datasourceschema.StringAttribute{Computed: true, MarkdownDescription: description}
		}
	}
	return datasourceschema.SetNestedAttribute{
		Computed:            true,
		NestedObject:        datasourceschema.NestedAttributeObject{Attributes: attrs},
		MarkdownDescription: regionsDescription,
	}
}

func distributionDataAttribute() datasourceschema.Attribute {
	return datasourceschema.SingleNestedAttribute{
		Computed: true,
		Attributes: map[string]datasourceschema.Attribute{
			"mode": datasourceschema.StringAttribute{Computed: true, MarkdownDescription: "`standby`."},
			"region": datasourceschema.SetNestedAttribute{
				Computed: true,
				NestedObject: datasourceschema.NestedAttributeObject{Attributes: map[string]datasourceschema.Attribute{
					"cloud":   datasourceschema.StringAttribute{Computed: true, MarkdownDescription: "Cloud of the region."},
					"region":  datasourceschema.StringAttribute{Computed: true, MarkdownDescription: "Region name."},
					"primary": datasourceschema.BoolAttribute{Computed: true, MarkdownDescription: "Whether this region holds the primary."},
				}},
				MarkdownDescription: "The regions the cluster runs in.",
			},
		},
		MarkdownDescription: "The standby spread across regions. Null for a single-region cluster, and for a PostgreSQL cluster with a stretched distribution, which Terraform does not model.",
	}
}

func regionsFromProto(regions []*serverv1.RegionStatus) types.Set {
	sorted := slices.Clone(regions)
	slices.SortFunc(sorted, func(a, b *serverv1.RegionStatus) int {
		return cmp.Or(cmp.Compare(a.GetCloud(), b.GetCloud()), cmp.Compare(a.GetRegion(), b.GetRegion()))
	})
	elements := make([]attr.Value, 0, len(sorted))
	for _, r := range sorted {
		elements = append(elements, types.ObjectValueMust(regionStatusType.AttrTypes, map[string]attr.Value{
			"cloud":            types.StringValue(r.GetCloud()),
			"region":           types.StringValue(r.GetRegion()),
			"role":             types.StringValue(r.GetRole()),
			"phase":            types.StringValue(r.GetPhase()),
			"ready_nodes":      types.Int64Value(int64(r.GetReadyNodes())),
			"total_nodes":      types.Int64Value(int64(r.GetTotalNodes())),
			"replication_link": types.StringValue(r.GetReplicationLink()),
			"held":             types.StringValue(r.GetHeld()),
			"message":          types.StringValue(r.GetMessage()),
		}))
	}
	return types.SetValueMust(regionStatusType, elements)
}
