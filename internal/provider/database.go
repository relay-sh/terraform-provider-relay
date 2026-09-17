package provider

import (
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

const (
	phaseRunning = "running"
	phaseFailed  = "failed"
	phaseDeleted = "deleted"
)

type nodeModel struct {
	Role      types.String   `tfsdk:"role"`
	Count     types.Int64    `tfsdk:"count"`
	Provider  types.String   `tfsdk:"provider"`
	Region    types.String   `tfsdk:"region"`
	Tier      types.String   `tfsdk:"tier"`
	Resources resourcesModel `tfsdk:"resources"`
	Tags      types.Map      `tfsdk:"tags"`
}

type resourcesModel struct {
	CPU          types.String `tfsdk:"cpu"`
	Memory       types.String `tfsdk:"memory"`
	Storage      types.String `tfsdk:"storage"`
	StorageClass types.String `tfsdk:"storage_class"`
	IOPS         types.Int64  `tfsdk:"iops"`
	Throughput   types.Int64  `tfsdk:"throughput"`
}

var (
	replicationAttrTypes = map[string]attr.Type{"mode": types.StringType, "synchronous_replicas": types.Int64Type}
	endpointAttrTypes    = map[string]attr.Type{"role": types.StringType, "host": types.StringType, "port": types.Int64Type}
)

func nodesToProto(nodes []nodeModel) []*serverv1.NodeSpec {
	out := make([]*serverv1.NodeSpec, 0, len(nodes))
	for _, n := range nodes {
		res := &serverv1.ResourceRequirements{
			Storage:      n.Resources.Storage.ValueString(),
			StorageClass: n.Resources.StorageClass.ValueString(),
		}
		if v := n.Resources.IOPS; !v.IsNull() && !v.IsUnknown() {
			res.Iops = int32Ptr(v.ValueInt64())
		}
		if v := n.Resources.Throughput; !v.IsNull() && !v.IsUnknown() {
			res.Throughput = int32Ptr(v.ValueInt64())
		}
		out = append(out, &serverv1.NodeSpec{
			Role:      n.Role.ValueString(),
			Count:     int32(n.Count.ValueInt64()),
			Provider:  n.Provider.ValueString(),
			Region:    n.Region.ValueString(),
			Tier:      n.Tier.ValueString(),
			Resources: res,
			Tags:      stringMapToGo(n.Tags),
		})
	}
	return out
}

func nodesFromProto(nodes []*serverv1.NodeSpec) []nodeModel {
	out := make([]nodeModel, 0, len(nodes))
	for _, n := range nodes {
		res := n.GetResources()
		out = append(out, nodeModel{
			Role:     types.StringValue(n.GetRole()),
			Count:    types.Int64Value(int64(n.GetCount())),
			Provider: types.StringValue(n.GetProvider()),
			Region:   types.StringValue(n.GetRegion()),
			Tier:     types.StringValue(n.GetTier()),
			Resources: resourcesModel{
				CPU:          types.StringValue(res.GetCpu()),
				Memory:       types.StringValue(res.GetMemory()),
				Storage:      types.StringValue(res.GetStorage()),
				StorageClass: types.StringValue(res.GetStorageClass()),
				IOPS:         optionalInt64(res.Iops),
				Throughput:   optionalInt64(res.Throughput),
			},
			Tags: stringMapFromGo(n.GetTags()),
		})
	}
	return out
}

func replicationToProto(r types.Object) *serverv1.Replication {
	if r.IsNull() || r.IsUnknown() {
		return nil
	}
	attrs := r.Attributes()
	out := &serverv1.Replication{Mode: attrs["mode"].(types.String).ValueString()}
	if v := attrs["synchronous_replicas"].(types.Int64); !v.IsNull() && !v.IsUnknown() {
		out.SynchronousReplicas = int32Ptr(v.ValueInt64())
	}
	return out
}

func replicationFromProto(r *serverv1.Replication) types.Object {
	if r == nil {
		return types.ObjectNull(replicationAttrTypes)
	}
	return types.ObjectValueMust(replicationAttrTypes, map[string]attr.Value{
		"mode":                 types.StringValue(r.GetMode()),
		"synchronous_replicas": optionalInt64(r.SynchronousReplicas),
	})
}

func endpointsFromProto(eps []*serverv1.PublicEndpoint) types.List {
	elemType := types.ObjectType{AttrTypes: endpointAttrTypes}
	elems := make([]attr.Value, 0, len(eps))
	for _, e := range eps {
		elems = append(elems, types.ObjectValueMust(endpointAttrTypes, map[string]attr.Value{
			"role": types.StringValue(e.GetRole()),
			"host": types.StringValue(e.GetHost()),
			"port": types.Int64Value(int64(e.GetPort())),
		}))
	}
	return types.ListValueMust(elemType, elems)
}

func stringMapToGo(m types.Map) map[string]string {
	if m.IsNull() || m.IsUnknown() {
		return nil
	}
	out := make(map[string]string, len(m.Elements()))
	for k, v := range m.Elements() {
		out[k] = v.(types.String).ValueString()
	}
	return out
}

func stringMapFromGo(m map[string]string) types.Map {
	elems := make(map[string]attr.Value, len(m))
	for k, v := range m {
		elems[k] = types.StringValue(v)
	}
	return types.MapValueMust(types.StringType, elems)
}

func optionalInt64(v *int32) types.Int64 {
	if v == nil {
		return types.Int64Null()
	}
	return types.Int64Value(int64(*v))
}

func int32Ptr(v int64) *int32 {
	out := int32(v)
	return &out
}
