package provider

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

type privateEndpointServiceResource struct {
	client *client.Client
}

type privateEndpointServiceResourceModel struct {
	privateEndpointServiceModel
	Timeouts timeouts.Value `tfsdk:"timeouts"`
}

func newPrivateEndpointServiceResource() resource.Resource {
	return &privateEndpointServiceResource{}
}

func (r *privateEndpointServiceResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_private_endpoint_service"
}

func (r *privateEndpointServiceResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	keep := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "The workspace's private endpoint service in one cloud region: what a `relay_private_endpoint` and the cloud-side endpoint point at. " +
			"One per workspace, cloud and region. Destroying it waits for its endpoints to be gone.",
		Attributes: map[string]schema.Attribute{
			"id":           schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "Service ID."},
			"workspace_id": schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Workspace the service belongs to."},
			"cloud": schema.StringAttribute{
				Required:            true,
				PlanModifiers:       replace,
				Validators:          []validator.String{stringvalidator.OneOf(cloudAWS)},
				MarkdownDescription: "Cloud provider. Only `aws` is served today.",
			},
			"region": schema.StringAttribute{
				Required:            true,
				PlanModifiers:       replace,
				MarkdownDescription: "Cloud region, for example `us-east-1`. Must be one the platform serves private endpoints in.",
			},
			"status": schema.StringAttribute{Computed: true, MarkdownDescription: "`INITIATING`, `AVAILABLE`, `FAILED` or `DELETING`."},
			"endpoint_service_name": schema.StringAttribute{
				Computed:            true,
				PlanModifiers:       keep,
				MarkdownDescription: "AWS VPC endpoint service name (`com.amazonaws.vpce.<region>.vpce-svc-…`), the `service_name` of an `aws_vpc_endpoint`.",
			},
			"error_message": schema.StringAttribute{Computed: true, MarkdownDescription: "Why the service failed, when it did."},
			"created_at":    schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "Creation time, RFC 3339."},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx, timeouts.Opts{Create: true, Delete: true}),
		},
	}
}

func (r *privateEndpointServiceResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *privateEndpointServiceResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan privateEndpointServiceResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	timeout, diags := plan.Timeouts.Create(ctx, 10*time.Minute)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	workspaceID := plan.WorkspaceID.ValueString()
	out, err := r.client.API.CreatePrivateEndpointService(r.client.Workspace(ctx, workspaceID), &serverv1.CreatePrivateEndpointServiceRequest{
		Provider: plan.Cloud.ValueString(),
		Region:   plan.Region.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Cannot create private endpoint service", err.Error())
		return
	}
	svc := out.GetService()
	state := privateEndpointServiceResourceModel{privateEndpointServiceFromProto(plan.WorkspaceID, svc), plan.Timeouts}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)

	err = waitFor(ctx, timeout, func(ctx context.Context) (bool, error) {
		if svc.GetStatus() == endpointStatusFailed {
			return false, errors.New(svc.GetErrorMessage())
		}
		if svc.GetStatus() == endpointStatusAvailable {
			return true, nil
		}
		next, err := privateEndpointService(ctx, r.client, workspaceID, svc.GetId())
		if err == nil {
			svc = next
		}
		return false, err
	})
	state.privateEndpointServiceModel = privateEndpointServiceFromProto(plan.WorkspaceID, svc)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	if err != nil {
		resp.Diagnostics.AddError("Private endpoint service did not become available", err.Error())
	}
}

func (r *privateEndpointServiceResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state privateEndpointServiceResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	svc, err := privateEndpointService(ctx, r.client, state.WorkspaceID.ValueString(), state.ID.ValueString())
	if relay.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read private endpoint service", err.Error())
		return
	}
	state.privateEndpointServiceModel = privateEndpointServiceFromProto(state.WorkspaceID, svc)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *privateEndpointServiceResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior privateEndpointServiceResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	prior.Timeouts = plan.Timeouts
	resp.Diagnostics.Append(resp.State.Set(ctx, &prior)...)
}

func (r *privateEndpointServiceResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state privateEndpointServiceResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	timeout, diags := state.Timeouts.Delete(ctx, 10*time.Minute)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	scoped := r.client.Workspace(ctx, state.WorkspaceID.ValueString())
	err := waitFor(scoped, timeout, func(ctx context.Context) (bool, error) {
		_, err := r.client.API.DeletePrivateEndpointService(ctx, &serverv1.DeletePrivateEndpointServiceRequest{Id: state.ID.ValueString()})
		switch {
		case err == nil, relay.IsNotFound(err):
			return true, nil
		case status.Code(err) == codes.FailedPrecondition:
			return false, nil
		}
		return false, err
	})
	if err != nil {
		resp.Diagnostics.AddError("Cannot delete private endpoint service", err.Error())
	}
}

func (r *privateEndpointServiceResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, "/")
	if slices.Contains(parts, "") || (len(parts) != 2 && len(parts) != 3) {
		resp.Diagnostics.AddError("Invalid import ID", "Expected <workspace_id>/<cloud>/<region> or <workspace_id>/<service_id>, got "+req.ID+".")
		return
	}
	id := parts[len(parts)-1]
	if len(parts) == 3 {
		svc, err := privateEndpointServiceIn(ctx, r.client, parts[0], parts[1], parts[2])
		if err != nil {
			resp.Diagnostics.AddError("Cannot list private endpoint services", err.Error())
			return
		}
		if svc == nil {
			resp.Diagnostics.AddError("Private endpoint service not found", "Workspace "+parts[0]+" has no service in "+parts[1]+"/"+parts[2]+".")
			return
		}
		id = svc.GetId()
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("workspace_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
}
