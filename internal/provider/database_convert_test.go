package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

func TestNodesRoundTripOptionalLimits(t *testing.T) {
	nodes := []nodeModel{{
		Role: types.StringValue("data"), Count: types.Int64Value(1),
		Provider: types.StringValue("aws"), Region: types.StringValue("us-east-1"),
		Resources: resourcesModel{
			CPU: types.StringValue("1"), Memory: types.StringValue("1Gi"), Storage: types.StringValue("1Gi"),
			StorageClass: types.StringUnknown(), IOPS: types.Int64Unknown(), Throughput: types.Int64Value(250),
		},
		Tags: types.MapUnknown(types.StringType),
	}}
	out := nodesToProto(nodes)
	res := out[0].GetResources()
	if res.Iops != nil || res.GetThroughput() != 250 || out[0].GetTags() != nil {
		t.Fatalf("unknown limits must be omitted and known ones sent: %+v", out[0])
	}
	back := nodesFromProto(out)
	if back[0].Resources.Throughput.ValueInt64() != 250 || !back[0].Resources.IOPS.IsNull() || len(back[0].Tags.Elements()) != 0 {
		t.Fatalf("round trip = %+v", back[0])
	}
}

func TestReplicationConversions(t *testing.T) {
	if replicationToProto(types.ObjectNull(replicationAttrTypes)) != nil || replicationToProto(types.ObjectUnknown(replicationAttrTypes)) != nil {
		t.Error("null or unknown replication must not be sent")
	}
	async := types.ObjectValueMust(replicationAttrTypes, map[string]attr.Value{"mode": types.StringValue("async"), "synchronous_replicas": types.Int64Null()})
	if got := replicationToProto(async); got.GetMode() != "async" || got.SynchronousReplicas != nil {
		t.Errorf("async replication = %+v", got)
	}
	if !replicationFromProto(nil).IsNull() {
		t.Error("absent replication must read as null")
	}
	if got := replicationFromProto(&serverv1.Replication{Mode: "async"}); got.Attributes()["mode"].(types.String).ValueString() != "async" {
		t.Errorf("replication = %v", got)
	}
}

func TestStringMapToGo(t *testing.T) {
	if stringMapToGo(types.MapNull(types.StringType)) != nil || stringMapToGo(types.MapUnknown(types.StringType)) != nil {
		t.Error("null or unknown maps must not be sent")
	}
}
