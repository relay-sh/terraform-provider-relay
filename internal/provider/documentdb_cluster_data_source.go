package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

type documentDBClusterDataSource struct {
	client *client.Client
}

type documentDBClusterDataModel struct {
	ID                 types.String                        `tfsdk:"id"`
	WorkspaceID        types.String                        `tfsdk:"workspace_id"`
	Name               types.String                        `tfsdk:"name"`
	Version            types.String                        `tfsdk:"version"`
	Nodes              map[string]documentDBNodeGroupModel `tfsdk:"nodes"`
	Storage            types.Object                        `tfsdk:"storage"`
	Phase              types.String                        `tfsdk:"phase"`
	BackupsEnabled     types.Bool                          `tfsdk:"backups_enabled"`
	DeletionProtection types.Bool                          `tfsdk:"deletion_protection"`
	EndpointHost       types.String                        `tfsdk:"endpoint_host"`
	EndpointPort       types.Int64                         `tfsdk:"endpoint_port"`
	PublicEndpoints    types.List                          `tfsdk:"public_endpoints"`
	ReadyNodes         types.Int64                         `tfsdk:"ready_nodes"`
	TotalNodes         types.Int64                         `tfsdk:"total_nodes"`
}

func newDocumentDBClusterDataSource() datasource.DataSource {
	return &documentDBClusterDataSource{}
}

func (d *documentDBClusterDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_documentdb_cluster"
}

func (d *documentDBClusterDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	endpoint := schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
		"role": schema.StringAttribute{Computed: true},
		"host": schema.StringAttribute{Computed: true},
		"port": schema.Int64Attribute{Computed: true},
	}}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads an existing managed DocumentDB cluster in a workspace, by ID or by name.",
		Attributes: map[string]schema.Attribute{
			"workspace_id": schema.StringAttribute{Required: true, MarkdownDescription: "Workspace to look in."},
			"id":           schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Cluster ID."},
			"name":         schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Cluster name."},
			"version":      schema.StringAttribute{Computed: true, MarkdownDescription: "DocumentDB version, `17` or `18`."},
			"nodes": schema.MapNestedAttribute{
				Computed: true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"count":  schema.Int64Attribute{Computed: true},
					"cloud":  schema.StringAttribute{Computed: true},
					"region": schema.StringAttribute{Computed: true},
					"tier":   schema.StringAttribute{Computed: true},
					"resources": schema.SingleNestedAttribute{
						Computed: true,
						Attributes: map[string]schema.Attribute{
							"cpu":    schema.StringAttribute{Computed: true},
							"memory": schema.StringAttribute{Computed: true},
						},
					},
				}},
				MarkdownDescription: "The cluster's node group, keyed by its label.",
			},
			"storage": schema.SingleNestedAttribute{
				Computed:            true,
				MarkdownDescription: "Volume size of every node.",
				Attributes:          map[string]schema.Attribute{"size": schema.StringAttribute{Computed: true}},
			},
			"phase":               schema.StringAttribute{Computed: true},
			"backups_enabled":     schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the cluster takes scheduled backups."},
			"deletion_protection": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the platform refuses to delete the cluster."},
			"endpoint_host":       schema.StringAttribute{Computed: true, MarkdownDescription: "Public read-write endpoint host. Empty until the platform publishes it."},
			"endpoint_port":       schema.Int64Attribute{Computed: true, MarkdownDescription: "Public read-write endpoint port, 27017."},
			"public_endpoints":    schema.ListNestedAttribute{Computed: true, NestedObject: endpoint, MarkdownDescription: "Public endpoints by role: `rw` and `ro`."},
			"ready_nodes":         schema.Int64Attribute{Computed: true},
			"total_nodes":         schema.Int64Attribute{Computed: true},
		},
	}
}

func (d *documentDBClusterDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return exactlyOneOf("id", "name")
}

func (d *documentDBClusterDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *documentDBClusterDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config documentDBClusterDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	scoped := d.client.Workspace(ctx, config.WorkspaceID.ValueString())

	var db *serverv1.Database
	if !config.ID.IsNull() {
		out, err := d.client.API.GetDatabase(scoped, &serverv1.GetDatabaseRequest{Id: config.ID.ValueString()})
		if err != nil && !relay.IsNotFound(err) {
			resp.Diagnostics.AddError("Cannot read cluster", err.Error())
			return
		}
		db = out.GetDatabase()
	} else {
		found, err := findDatabaseByName(scoped, d.client, config.Name.ValueString(), engineDocumentDB)
		if err != nil {
			resp.Diagnostics.AddError("Cannot list clusters", err.Error())
			return
		}
		db = found
	}
	if db.GetSpec().GetEngine() != engineDocumentDB {
		resp.Diagnostics.AddError("Cluster not found", "No DocumentDB cluster matches in the workspace.")
		return
	}

	full := documentDBClusterFromProto(db, documentDBClusterModel{WorkspaceID: config.WorkspaceID})
	state := documentDBClusterDataModel{
		ID:                 full.ID,
		WorkspaceID:        full.WorkspaceID,
		Name:               full.Name,
		Version:            full.Version,
		Nodes:              full.Nodes,
		Storage:            full.Storage,
		Phase:              full.Phase,
		BackupsEnabled:     full.BackupsEnabled,
		DeletionProtection: full.DeletionProtection,
		EndpointHost:       full.EndpointHost,
		EndpointPort:       full.EndpointPort,
		PublicEndpoints:    full.PublicEndpoints,
		ReadyNodes:         full.ReadyNodes,
		TotalNodes:         full.TotalNodes,
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
