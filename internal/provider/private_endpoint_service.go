package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

const (
	cloudAWS                = "aws"
	endpointStatusAvailable = "AVAILABLE"
	endpointStatusFailed    = "FAILED"
)

type privateEndpointServiceModel struct {
	ID                  types.String `tfsdk:"id"`
	WorkspaceID         types.String `tfsdk:"workspace_id"`
	Cloud               types.String `tfsdk:"cloud"`
	Region              types.String `tfsdk:"region"`
	Status              types.String `tfsdk:"status"`
	EndpointServiceName types.String `tfsdk:"endpoint_service_name"`
	ErrorMessage        types.String `tfsdk:"error_message"`
	CreatedAt           types.String `tfsdk:"created_at"`
}

func privateEndpointServiceFromProto(workspaceID types.String, s *serverv1.PrivateEndpointService) privateEndpointServiceModel {
	return privateEndpointServiceModel{
		ID:                  types.StringValue(s.GetId()),
		WorkspaceID:         workspaceID,
		Cloud:               types.StringValue(s.GetProvider()),
		Region:              types.StringValue(s.GetRegion()),
		Status:              types.StringValue(s.GetStatus()),
		EndpointServiceName: types.StringValue(s.GetEndpointServiceName()),
		ErrorMessage:        types.StringValue(s.GetErrorMessage()),
		CreatedAt:           timestampValue(s.GetCreatedAt()),
	}
}

func privateEndpointService(ctx context.Context, c *client.Client, workspaceID, id string) (*serverv1.PrivateEndpointService, error) {
	out, err := c.API.GetPrivateEndpointService(c.Workspace(ctx, workspaceID), &serverv1.GetPrivateEndpointServiceRequest{Id: id})
	return out.GetService(), err
}

func privateEndpointServiceIn(ctx context.Context, c *client.Client, workspaceID, cloud, region string) (*serverv1.PrivateEndpointService, error) {
	out, err := c.API.ListPrivateEndpointServices(c.Workspace(ctx, workspaceID), &serverv1.ListPrivateEndpointServicesRequest{})
	if err != nil {
		return nil, err
	}
	for _, s := range out.GetServices() {
		if s.GetProvider() == cloud && s.GetRegion() == region {
			return s, nil
		}
	}
	return nil, nil
}
