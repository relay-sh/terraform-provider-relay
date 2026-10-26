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

type valkeyClusterDataSource struct {
	client *client.Client
}

type valkeyClusterDataModel struct {
	ID               types.String                    `tfsdk:"id"`
	WorkspaceID      types.String                    `tfsdk:"workspace_id"`
	Name             types.String                    `tfsdk:"name"`
	Version          types.String                    `tfsdk:"version"`
	Mode             types.String                    `tfsdk:"mode"`
	Shards           types.Int64                     `tfsdk:"shards"`
	ReplicasPerShard types.Int64                     `tfsdk:"replicas_per_shard"`
	Nodes            map[string]valkeyNodeGroupModel `tfsdk:"nodes"`
	Parameters       types.Map                       `tfsdk:"parameters"`
	Phase            types.String                    `tfsdk:"phase"`
	EndpointHost     types.String                    `tfsdk:"endpoint_host"`
	EndpointPort     types.Int64                     `tfsdk:"endpoint_port"`
	PublicEndpoints  types.List                      `tfsdk:"public_endpoints"`
	PrivateEndpoints types.List                      `tfsdk:"private_endpoints"`
	StorageStatus    types.List                      `tfsdk:"storage_status"`
	ReadyNodes       types.Int64                     `tfsdk:"ready_nodes"`
	TotalNodes       types.Int64                     `tfsdk:"total_nodes"`
}

func newValkeyClusterDataSource() datasource.DataSource {
	return &valkeyClusterDataSource{}
}

func (d *valkeyClusterDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_valkey_cluster"
}

func (d *valkeyClusterDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	endpoint := schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
		"role": schema.StringAttribute{Computed: true},
		"host": schema.StringAttribute{Computed: true},
		"port": schema.Int64Attribute{Computed: true},
	}}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads an existing managed Valkey cluster in a workspace, by ID or by name.",
		Attributes: map[string]schema.Attribute{
			"workspace_id":       schema.StringAttribute{Required: true, MarkdownDescription: "Workspace to look in."},
			"id":                 schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Cluster ID."},
			"name":               schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Cluster name."},
			"version":            schema.StringAttribute{Computed: true},
			"mode":               schema.StringAttribute{Computed: true, MarkdownDescription: "`replication` or `cluster`."},
			"shards":             schema.Int64Attribute{Computed: true, MarkdownDescription: "Primaries the keyspace is split across. Null in `replication` mode."},
			"replicas_per_shard": schema.Int64Attribute{Computed: true, MarkdownDescription: "Replicas behind each shard's primary. Null in `replication` mode."},
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
							"cpu":     schema.StringAttribute{Computed: true},
							"memory":  schema.StringAttribute{Computed: true},
							"storage": schema.StringAttribute{Computed: true},
							"storage_autoscale": schema.SingleNestedAttribute{
								Computed: true,
								Attributes: map[string]schema.Attribute{
									"enabled":           schema.BoolAttribute{Computed: true},
									"threshold_percent": schema.Int64Attribute{Computed: true},
									"increase_percent":  schema.Int64Attribute{Computed: true},
									"max_storage":       schema.StringAttribute{Computed: true},
								},
							},
						},
					},
				}},
				MarkdownDescription: "The cluster's node group, keyed by its label.",
			},
			"parameters":        schema.MapAttribute{ElementType: types.StringType, Computed: true, MarkdownDescription: "Valkey settings."},
			"phase":             schema.StringAttribute{Computed: true},
			"endpoint_host":     schema.StringAttribute{Computed: true, MarkdownDescription: "Public read-write endpoint host. Empty until the platform publishes it."},
			"endpoint_port":     schema.Int64Attribute{Computed: true, MarkdownDescription: "Public read-write endpoint port."},
			"public_endpoints":  schema.ListNestedAttribute{Computed: true, NestedObject: endpoint},
			"private_endpoints": schema.ListNestedAttribute{Computed: true, NestedObject: endpoint},
			"storage_status": schema.ListNestedAttribute{
				Computed: true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"node_group": schema.StringAttribute{Computed: true},
					"size":       schema.StringAttribute{Computed: true},
				}},
			},
			"ready_nodes": schema.Int64Attribute{Computed: true},
			"total_nodes": schema.Int64Attribute{Computed: true},
		},
	}
}

func (d *valkeyClusterDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return exactlyOneOf("id", "name")
}

func (d *valkeyClusterDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *valkeyClusterDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config valkeyClusterDataModel
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
		found, err := d.findByName(scoped, config.Name.ValueString())
		if err != nil {
			resp.Diagnostics.AddError("Cannot list clusters", err.Error())
			return
		}
		db = found
	}
	if db.GetSpec().GetEngine() != engineValkey {
		resp.Diagnostics.AddError("Cluster not found", "No Valkey cluster matches in the workspace.")
		return
	}

	full := valkeyClusterFromProto(db, valkeyClusterModel{WorkspaceID: config.WorkspaceID, ReplicasPerShard: types.Int64Value(0)})
	state := valkeyClusterDataModel{
		ID:               full.ID,
		WorkspaceID:      full.WorkspaceID,
		Name:             full.Name,
		Version:          full.Version,
		Mode:             full.Mode,
		Shards:           full.Shards,
		ReplicasPerShard: full.ReplicasPerShard,
		Nodes:            full.Nodes,
		Parameters:       full.Parameters,
		Phase:            full.Phase,
		EndpointHost:     full.EndpointHost,
		EndpointPort:     full.EndpointPort,
		PublicEndpoints:  full.PublicEndpoints,
		PrivateEndpoints: full.PrivateEndpoints,
		StorageStatus:    full.StorageStatus,
		ReadyNodes:       full.ReadyNodes,
		TotalNodes:       full.TotalNodes,
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (d *valkeyClusterDataSource) findByName(ctx context.Context, name string) (*serverv1.Database, error) {
	token := ""
	for {
		out, err := d.client.API.ListDatabases(ctx, &serverv1.ListDatabasesRequest{PageToken: token})
		if err != nil {
			return nil, err
		}
		for _, db := range out.GetDatabases() {
			if db.GetName() == name && db.GetSpec().GetEngine() == engineValkey {
				return db, nil
			}
		}
		token = out.GetNextPageToken()
		if token == "" {
			return nil, nil
		}
	}
}
