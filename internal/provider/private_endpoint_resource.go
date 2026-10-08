package provider

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

type privateEndpointResource struct {
	client *client.Client
}

type privateEndpointResourceModel struct {
	privateEndpointModel
	Timeouts timeouts.Value `tfsdk:"timeouts"`
}

func newPrivateEndpointResource() resource.Resource {
	return &privateEndpointResource{}
}

func (r *privateEndpointResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_private_endpoint"
}

func (r *privateEndpointResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	keep := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Registers a cloud-side endpoint on a `relay_private_endpoint_service`. The platform accepts the connection on its side; " +
			"creating waits until it is `AVAILABLE` and the workspace's databases answer on `<name>-pl-<index>` hostnames from inside the endpoint's network. " +
			"Destroying rejects the connection and waits until the registration is gone. AWS never accepts a rejected VPC endpoint again, so registering the same `vpc_endpoint_id` " +
			"after its registration was destroyed fails with `REJECTED`; replace the `aws_vpc_endpoint` with it, for example with `replace_triggered_by`.",
		Attributes: map[string]schema.Attribute{
			"id":           schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "Endpoint ID."},
			"workspace_id": schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Workspace the service belongs to."},
			"service_id":   schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "`relay_private_endpoint_service` to register on."},
			"vpc_endpoint_id": schema.StringAttribute{
				Required:            true,
				PlanModifiers:       replace,
				Validators:          []validator.String{stringvalidator.RegexMatches(vpcEndpointID, "must be an AWS VPC endpoint ID (vpce-…)")},
				MarkdownDescription: "AWS interface VPC endpoint ID (`vpce-…`), usually `aws_vpc_endpoint.<name>.id`.",
			},
			"cloud":  schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "Cloud provider, from the service."},
			"region": schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "Cloud region, from the service."},
			"index": schema.Int64Attribute{
				Computed:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
				MarkdownDescription: "Per-workspace index in the endpoint's hostnames (`<name>-pl-<index>`). Never reused.",
			},
			"connection_status": schema.StringAttribute{Computed: true, MarkdownDescription: "`PENDING`, `PENDING_ACCEPTANCE`, `AVAILABLE`, `REJECTED`, `FAILED` or `DELETING`."},
			"dns_name":          schema.StringAttribute{Computed: true, MarkdownDescription: "Regional DNS name of the cloud endpoint, which the `-pl-<index>` hostnames point at."},
			"error_message":     schema.StringAttribute{Computed: true, MarkdownDescription: "Why the connection failed, when it did."},
			"created_at":        schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "Creation time, RFC 3339."},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx, timeouts.Opts{Create: true, Delete: true}),
		},
	}
}

func (r *privateEndpointResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *privateEndpointResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan privateEndpointResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	timeout, diags := plan.Timeouts.Create(ctx, 20*time.Minute)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	workspaceID, serviceID := plan.WorkspaceID.ValueString(), plan.ServiceID.ValueString()
	out, err := r.client.API.CreatePrivateEndpoint(r.client.Workspace(ctx, workspaceID), &serverv1.CreatePrivateEndpointRequest{
		ServiceId:           serviceID,
		InterfaceEndpointId: plan.VPCEndpointID.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Cannot register private endpoint", err.Error())
		return
	}
	ep := out.GetEndpoint()
	state := privateEndpointResourceModel{privateEndpointFromProto(plan.WorkspaceID, ep), plan.Timeouts}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)

	err = waitFor(ctx, timeout, func(ctx context.Context) (bool, error) {
		next, err := privateEndpoint(ctx, r.client, workspaceID, serviceID, ep.GetId())
		if err != nil {
			return false, err
		}
		ep = next
		switch ep.GetConnectionStatus() {
		case endpointStatusAvailable:
			return true, nil
		case endpointStatusRejected, endpointStatusFailed:
			return false, errors.New(ep.GetConnectionStatus() + ": " + ep.GetErrorMessage())
		}
		return false, nil
	})
	state.privateEndpointModel = privateEndpointFromProto(plan.WorkspaceID, ep)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	if err != nil {
		detail := err.Error()
		switch ep.GetConnectionStatus() {
		case endpointStatusRejected:
			detail += ". AWS does not accept a rejected VPC endpoint again, and removing a registration rejects it: replace the aws_vpc_endpoint."
		case endpointStatusFailed:
		default:
			detail += ". The cloud endpoint must exist and request a connection to the service's endpoint_service_name."
		}
		resp.Diagnostics.AddError("Private endpoint did not become available", detail)
	}
}

func (r *privateEndpointResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state privateEndpointResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	ep, err := privateEndpoint(ctx, r.client, state.WorkspaceID.ValueString(), state.ServiceID.ValueString(), state.ID.ValueString())
	if relay.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read private endpoint", err.Error())
		return
	}
	state.privateEndpointModel = privateEndpointFromProto(state.WorkspaceID, ep)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *privateEndpointResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior privateEndpointResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	prior.Timeouts = plan.Timeouts
	resp.Diagnostics.Append(resp.State.Set(ctx, &prior)...)
}

func (r *privateEndpointResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state privateEndpointResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	timeout, diags := state.Timeouts.Delete(ctx, 10*time.Minute)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	workspaceID, serviceID, id := state.WorkspaceID.ValueString(), state.ServiceID.ValueString(), state.ID.ValueString()
	_, err := r.client.API.DeletePrivateEndpoint(r.client.Workspace(ctx, workspaceID), &serverv1.DeletePrivateEndpointRequest{ServiceId: serviceID, Id: id})
	if relay.IsNotFound(err) {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot delete private endpoint", err.Error())
		return
	}
	err = waitFor(ctx, timeout, func(ctx context.Context) (bool, error) {
		_, err := privateEndpoint(ctx, r.client, workspaceID, serviceID, id)
		if relay.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
	if err != nil {
		resp.Diagnostics.AddError("Private endpoint was not deleted", err.Error())
	}
}

func (r *privateEndpointResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		resp.Diagnostics.AddError("Invalid import ID", "Expected <workspace_id>/<service_id>/<endpoint_id> or <workspace_id>/<service_id>/<vpc_endpoint_id>, got "+req.ID+".")
		return
	}
	id := parts[2]
	if vpcEndpointID.MatchString(id) {
		ep, err := privateEndpointByVPCEndpoint(ctx, r.client, parts[0], parts[1], id)
		if err != nil {
			resp.Diagnostics.AddError("Cannot read private endpoint service", err.Error())
			return
		}
		if ep == nil {
			resp.Diagnostics.AddError("Private endpoint not found", id+" is not registered on service "+parts[1]+".")
			return
		}
		id = ep.GetId()
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("workspace_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("service_id"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
}
