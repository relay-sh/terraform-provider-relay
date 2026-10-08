package provider

import (
	"context"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

var vpcEndpointID = regexp.MustCompile(`^vpce-[0-9a-f]{8}([0-9a-f]{9})?$`)

type privateEndpointModel struct {
	ID               types.String `tfsdk:"id"`
	WorkspaceID      types.String `tfsdk:"workspace_id"`
	ServiceID        types.String `tfsdk:"service_id"`
	VPCEndpointID    types.String `tfsdk:"vpc_endpoint_id"`
	Cloud            types.String `tfsdk:"cloud"`
	Region           types.String `tfsdk:"region"`
	Index            types.Int64  `tfsdk:"index"`
	ConnectionStatus types.String `tfsdk:"connection_status"`
	DNSName          types.String `tfsdk:"dns_name"`
	ErrorMessage     types.String `tfsdk:"error_message"`
	CreatedAt        types.String `tfsdk:"created_at"`
}

func privateEndpointFromProto(workspaceID types.String, e *serverv1.PrivateEndpoint) privateEndpointModel {
	return privateEndpointModel{
		ID:               types.StringValue(e.GetId()),
		WorkspaceID:      workspaceID,
		ServiceID:        types.StringValue(e.GetServiceId()),
		VPCEndpointID:    types.StringValue(e.GetInterfaceEndpointId()),
		Cloud:            types.StringValue(e.GetProvider()),
		Region:           types.StringValue(e.GetRegion()),
		Index:            types.Int64Value(int64(e.GetIndex())),
		ConnectionStatus: types.StringValue(e.GetConnectionStatus()),
		DNSName:          types.StringValue(e.GetDnsName()),
		ErrorMessage:     types.StringValue(e.GetErrorMessage()),
		CreatedAt:        timestampValue(e.GetCreatedAt()),
	}
}

func privateEndpoint(ctx context.Context, c *client.Client, workspaceID, serviceID, id string) (*serverv1.PrivateEndpoint, error) {
	out, err := c.API.GetPrivateEndpoint(c.Workspace(ctx, workspaceID), &serverv1.GetPrivateEndpointRequest{ServiceId: serviceID, Id: id})
	return out.GetEndpoint(), err
}

func privateEndpointByVPCEndpoint(ctx context.Context, c *client.Client, workspaceID, serviceID, vpcEndpoint string) (*serverv1.PrivateEndpoint, error) {
	svc, err := privateEndpointService(ctx, c, workspaceID, serviceID)
	if err != nil {
		return nil, err
	}
	for _, e := range svc.GetEndpoints() {
		if e.GetInterfaceEndpointId() == vpcEndpoint {
			return e, nil
		}
	}
	return nil, nil
}
