package provider

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

const (
	enginePostgreSQL = "postgresql"
	phaseRunning     = "running"
	phaseFailed      = "failed"
	phaseDeleted     = "deleted"
	tagNoSync        = "nosync"
	tagNoFailover    = "nofailover"
	tagTrue          = "true"
)

type nodeGroupModel struct {
	Count       types.Int64    `tfsdk:"count"`
	Cloud       types.String   `tfsdk:"cloud"`
	Region      types.String   `tfsdk:"region"`
	Tier        types.String   `tfsdk:"tier"`
	Resources   resourcesModel `tfsdk:"resources"`
	Synchronous types.Bool     `tfsdk:"synchronous"`
	Failover    types.Bool     `tfsdk:"failover"`
}

type resourcesModel struct {
	CPU          types.String `tfsdk:"cpu"`
	Memory       types.String `tfsdk:"memory"`
	Storage      types.String `tfsdk:"storage"`
	StorageClass types.String `tfsdk:"storage_class"`
	IOPS         types.Int64  `tfsdk:"iops"`
	Throughput   types.Int64  `tfsdk:"throughput"`
	DiskType     types.String `tfsdk:"disk_type"`
}

var (
	replicationAttrTypes = map[string]attr.Type{"mode": types.StringType, "synchronous_replicas": types.Int64Type}
	endpointAttrTypes    = map[string]attr.Type{"role": types.StringType, "host": types.StringType, "port": types.Int64Type}
)

func placements(ctx context.Context, c *client.Client) ([]*serverv1.DatabaseProvider, error) {
	out, err := c.API.ListDatabaseProviders(ctx, &serverv1.ListDatabaseProvidersRequest{})
	if err != nil {
		return nil, err
	}
	catalog := out.GetProviders()
	sort.Slice(catalog, func(i, j int) bool { return catalog[i].GetName() < catalog[j].GetName() })
	return catalog, nil
}

func provisionerFor(catalog []*serverv1.DatabaseProvider, cloud, region string) (string, error) {
	if cloud == "" && region == "" {
		return "", nil
	}
	available := make([]string, 0, len(catalog))
	for _, p := range catalog {
		if len(p.GetEngines()) > 0 && !slices.Contains(p.GetEngines(), enginePostgreSQL) {
			continue
		}
		if (cloud == "" || p.GetCloud() == cloud) && (region == "" || p.GetRegion() == region) {
			return p.GetName(), nil
		}
		available = append(available, p.GetCloud()+"/"+p.GetRegion())
	}
	return "", fmt.Errorf("no PostgreSQL provisioner serves cloud %q in region %q; the platform offers [%s]", cloud, region, strings.Join(available, " "))
}

func placementOf(catalog []*serverv1.DatabaseProvider, n *serverv1.NodeSpec) (string, string) {
	for _, p := range catalog {
		if p.GetName() == n.GetProvider() {
			return p.GetCloud(), p.GetRegion()
		}
	}
	return "", n.GetRegion()
}

func nodesToProto(nodes map[string]nodeGroupModel, catalog []*serverv1.DatabaseProvider) ([]*serverv1.NodeSpec, error) {
	names := make([]string, 0, len(nodes))
	for name := range nodes {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]*serverv1.NodeSpec, 0, len(names))
	for _, name := range names {
		n := nodes[name]
		provisioner, err := provisionerFor(catalog, n.Cloud.ValueString(), n.Region.ValueString())
		if err != nil {
			return nil, err
		}
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
		if v := n.Resources.DiskType; !v.IsNull() && !v.IsUnknown() {
			res.DiskType = v.ValueString()
		}
		out = append(out, &serverv1.NodeSpec{
			Role:      name,
			Count:     int32(n.Count.ValueInt64()),
			Provider:  provisioner,
			Region:    n.Region.ValueString(),
			Tier:      n.Tier.ValueString(),
			Resources: res,
			Tags:      patroniTags(n),
		})
	}
	return out, nil
}

func patroniTags(n nodeGroupModel) map[string]string {
	tags := map[string]string{}
	if !n.Synchronous.ValueBool() {
		tags[tagNoSync] = tagTrue
	}
	if !n.Failover.ValueBool() {
		tags[tagNoFailover] = tagTrue
	}
	if len(tags) == 0 {
		return nil
	}
	return tags
}

func nodesFromProto(nodes []*serverv1.NodeSpec, catalog []*serverv1.DatabaseProvider) map[string]nodeGroupModel {
	out := make(map[string]nodeGroupModel, len(nodes))
	for _, n := range nodes {
		res := n.GetResources()
		cloud, region := placementOf(catalog, n)
		out[n.GetRole()] = nodeGroupModel{
			Count:  types.Int64Value(int64(n.GetCount())),
			Cloud:  types.StringValue(cloud),
			Region: types.StringValue(region),
			Tier:   types.StringValue(n.GetTier()),
			Resources: resourcesModel{
				CPU:          types.StringValue(res.GetCpu()),
				Memory:       types.StringValue(res.GetMemory()),
				Storage:      types.StringValue(res.GetStorage()),
				StorageClass: types.StringValue(res.GetStorageClass()),
				IOPS:         optionalInt64(res.Iops),
				Throughput:   optionalInt64(res.Throughput),
				DiskType:     types.StringValue(res.GetDiskType()),
			},
			Synchronous: types.BoolValue(n.GetTags()[tagNoSync] != tagTrue),
			Failover:    types.BoolValue(n.GetTags()[tagNoFailover] != tagTrue),
		}
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
