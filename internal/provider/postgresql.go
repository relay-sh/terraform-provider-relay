package provider

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

const (
	enginePostgreSQL = "postgresql"
	phaseRunning     = "running"
	phaseFailed      = "failed"
	phaseDeleted     = "deleted"
	roleElectable    = "electable"
	roleStandby      = "standby"
	roleReadOnly     = "read_only"
)

type nodeGroupModel struct {
	Role      types.String   `tfsdk:"role"`
	Count     types.Int64    `tfsdk:"count"`
	Cloud     types.String   `tfsdk:"cloud"`
	Region    types.String   `tfsdk:"region"`
	Tier      types.String   `tfsdk:"tier"`
	Resources resourcesModel `tfsdk:"resources"`
}

type resourcesModel struct {
	CPU              types.String `tfsdk:"cpu"`
	Memory           types.String `tfsdk:"memory"`
	Storage          types.String `tfsdk:"storage"`
	IOPS             types.Int64  `tfsdk:"iops"`
	Throughput       types.Int64  `tfsdk:"throughput"`
	DiskType         types.String `tfsdk:"disk_type"`
	StorageAutoscale types.Object `tfsdk:"storage_autoscale"`
}

var (
	replicationAttrTypes      = map[string]attr.Type{"mode": types.StringType, "synchronous_replicas": types.Int64Type}
	endpointAttrTypes         = map[string]attr.Type{"role": types.StringType, "host": types.StringType, "port": types.Int64Type}
	storageAutoscaleAttrTypes = map[string]attr.Type{
		"enabled":           types.BoolType,
		"threshold_percent": types.Int64Type,
		"increase_percent":  types.Int64Type,
		"max_storage":       types.StringType,
	}
	storageStatusAttrTypes = map[string]attr.Type{"node_group": types.StringType, "size": types.StringType}
)

func nodesToProto(nodes map[string]nodeGroupModel) []*serverv1.NodeSpec {
	names := make([]string, 0, len(nodes))
	for name := range nodes {
		names = append(names, name)
	}
	// The platform reads a group's identity from its name, but the order still decides
	// which member each per-node hostname addresses, so keep it deterministic.
	sort.Slice(names, func(i, j int) bool {
		ri, rj := roleOrder(nodes[names[i]]), roleOrder(nodes[names[j]])
		if ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})

	out := make([]*serverv1.NodeSpec, 0, len(names))
	for _, name := range names {
		n := nodes[name]
		res := &serverv1.ResourceRequirements{
			Storage:          n.Resources.Storage.ValueString(),
			StorageAutoscale: storageAutoscaleToProto(n.Resources.StorageAutoscale),
		}
		if v := n.Resources.IOPS; !v.IsNull() && !v.IsUnknown() {
			res.Iops = int32Ptr(v.ValueInt64())
		}
		if v := n.Resources.Throughput; !v.IsNull() && !v.IsUnknown() {
			res.Throughput = int32Ptr(v.ValueInt64())
		}
		if v := n.Resources.DiskType; !v.IsNull() && !v.IsUnknown() {
			res.DiskType = v.ValueString()
		}
		out = append(out, &serverv1.NodeSpec{
			Name:      name,
			Role:      n.Role.ValueString(),
			Count:     int32(n.Count.ValueInt64()),
			Cloud:     n.Cloud.ValueString(),
			Region:    n.Region.ValueString(),
			Tier:      n.Tier.ValueString(),
			Resources: res,
		})
	}
	return out
}

// primaryNodeGroup keeps the group the platform already chose when it is still electable,
// so adding a group never moves the primary; otherwise it takes the first electable label.
func primaryNodeGroup(nodes map[string]nodeGroupModel, current types.String) string {
	if name := current.ValueString(); name != "" && !current.IsUnknown() {
		if g, ok := nodes[name]; ok && g.Role.ValueString() == roleElectable {
			return name
		}
	}
	labels := make([]string, 0, len(nodes))
	for name, g := range nodes {
		if g.Role.ValueString() == roleElectable {
			labels = append(labels, name)
		}
	}
	sort.Strings(labels)
	if len(labels) == 0 {
		return ""
	}
	return labels[0]
}

func roleOrder(n nodeGroupModel) int {
	switch n.Role.ValueString() {
	case roleElectable:
		return 0
	case roleStandby:
		return 1
	}
	return 2
}

func nodesFromProto(nodes []*serverv1.NodeSpec, prior map[string]nodeGroupModel) map[string]nodeGroupModel {
	out := make(map[string]nodeGroupModel, len(nodes))
	for _, n := range nodes {
		res := n.GetResources()
		priorAutoscale := types.ObjectNull(storageAutoscaleAttrTypes)
		if p, ok := prior[n.GetName()]; ok {
			priorAutoscale = p.Resources.StorageAutoscale
		}
		out[n.GetName()] = nodeGroupModel{
			Role:   types.StringValue(n.GetRole()),
			Count:  types.Int64Value(int64(n.GetCount())),
			Cloud:  types.StringValue(n.GetCloud()),
			Region: types.StringValue(n.GetRegion()),
			Tier:   types.StringValue(n.GetTier()),
			Resources: resourcesModel{
				CPU:              types.StringValue(res.GetCpu()),
				Memory:           types.StringValue(res.GetMemory()),
				Storage:          types.StringValue(res.GetStorage()),
				IOPS:             optionalInt64(res.Iops),
				Throughput:       optionalInt64(res.Throughput),
				DiskType:         types.StringValue(res.GetDiskType()),
				StorageAutoscale: storageAutoscaleFromProto(res.GetStorageAutoscale(), priorAutoscale),
			},
		}
	}
	return out
}

func storageAutoscaleToProto(o types.Object) *serverv1.StorageAutoscale {
	out := &serverv1.StorageAutoscale{}
	if o.IsNull() || o.IsUnknown() {
		return out
	}
	attrs := o.Attributes()
	out.Enabled = attrs["enabled"].(types.Bool).ValueBool()
	if v := attrs["threshold_percent"].(types.Int64); !v.IsNull() && !v.IsUnknown() {
		out.ThresholdPercent = int32(v.ValueInt64())
	}
	if v := attrs["increase_percent"].(types.Int64); !v.IsNull() && !v.IsUnknown() {
		out.IncreasePercent = int32(v.ValueInt64())
	}
	if v := attrs["max_storage"].(types.String); !v.IsNull() && !v.IsUnknown() {
		out.MaxStorage = v.ValueString()
	}
	return out
}

func storageAutoscaleFromProto(sa *serverv1.StorageAutoscale, prior types.Object) types.Object {
	if sa == nil || (!sa.GetEnabled() && prior.IsNull()) {
		return types.ObjectNull(storageAutoscaleAttrTypes)
	}
	return types.ObjectValueMust(storageAutoscaleAttrTypes, map[string]attr.Value{
		"enabled":           types.BoolValue(sa.GetEnabled()),
		"threshold_percent": types.Int64Value(int64(sa.GetThresholdPercent())),
		"increase_percent":  types.Int64Value(int64(sa.GetIncreasePercent())),
		"max_storage":       optionalString(sa.GetMaxStorage()),
	})
}

func storageStatusFromProto(entries []*serverv1.NodeGroupStorage) types.List {
	elemType := types.ObjectType{AttrTypes: storageStatusAttrTypes}
	elems := make([]attr.Value, 0, len(entries))
	for _, e := range entries {
		elems = append(elems, types.ObjectValueMust(storageStatusAttrTypes, map[string]attr.Value{
			"node_group": types.StringValue(e.GetNodeGroup()),
			"size":       types.StringValue(e.GetSize()),
		}))
	}
	return types.ListValueMust(elemType, elems)
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

func optionalString(v string) types.String {
	if v == "" {
		return types.StringNull()
	}
	return types.StringValue(v)
}

func int32Ptr(v int64) *int32 {
	out := int32(v)
	return &out
}

func timestampValue(ts *timestamppb.Timestamp) types.String {
	if !ts.IsValid() || ts.GetSeconds() <= 0 {
		return types.StringValue("")
	}
	return types.StringValue(ts.AsTime().UTC().Format(time.RFC3339))
}

var quantityScale = map[string]float64{
	"":   1,
	"m":  1e-3,
	"k":  1e3,
	"M":  1e6,
	"G":  1e9,
	"T":  1e12,
	"P":  1e15,
	"E":  1e18,
	"Ki": 1 << 10,
	"Mi": 1 << 20,
	"Gi": 1 << 30,
	"Ti": 1 << 40,
	"Pi": 1 << 50,
	"Ei": 1 << 60,
}

func parseQuantity(s string) (float64, bool) {
	number, suffix := s, ""
	if i := strings.IndexFunc(s, func(r rune) bool { return r != '.' && (r < '0' || r > '9') }); i >= 0 {
		number, suffix = s[:i], s[i:]
	}
	scale, ok := quantityScale[suffix]
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseFloat(number, 64)
	if err != nil {
		return 0, false
	}
	return v * scale, true
}

func shrinks(planned, current types.String) bool {
	if planned.IsUnknown() || current.IsUnknown() {
		return false
	}
	want, wantOK := parseQuantity(planned.ValueString())
	have, haveOK := parseQuantity(current.ValueString())
	return wantOK && haveOK && want < have
}
