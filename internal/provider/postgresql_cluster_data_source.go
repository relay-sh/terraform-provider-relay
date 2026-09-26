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

type postgresqlClusterDataSource struct {
	client *client.Client
}

type postgresqlClusterDataModel struct {
	ID               types.String `tfsdk:"id"`
	WorkspaceID      types.String `tfsdk:"workspace_id"`
	Name             types.String `tfsdk:"name"`
	Version          types.String `tfsdk:"version"`
	Phase            types.String `tfsdk:"phase"`
	EndpointHost     types.String `tfsdk:"endpoint_host"`
	EndpointPort     types.Int64  `tfsdk:"endpoint_port"`
	PublicEndpoints  types.List   `tfsdk:"public_endpoints"`
	PrivateEndpoints types.List   `tfsdk:"private_endpoints"`
	StorageStatus    types.List   `tfsdk:"storage_status"`
	ReadyNodes       types.Int64  `tfsdk:"ready_nodes"`
	TotalNodes       types.Int64  `tfsdk:"total_nodes"`
}

func newPostgreSQLClusterDataSource() datasource.DataSource {
	return &postgresqlClusterDataSource{}
}

func (d *postgresqlClusterDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_postgresql_cluster"
}

func (d *postgresqlClusterDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	endpoint := schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
		"role": schema.StringAttribute{Computed: true},
		"host": schema.StringAttribute{Computed: true},
		"port": schema.Int64Attribute{Computed: true},
	}}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads an existing managed PostgreSQL cluster in a workspace, by ID or by name.",
		Attributes: map[string]schema.Attribute{
			"workspace_id":      schema.StringAttribute{Required: true, MarkdownDescription: "Workspace to look in."},
			"id":                schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Cluster ID."},
			"name":              schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Cluster name."},
			"version":           schema.StringAttribute{Computed: true},
			"phase":             schema.StringAttribute{Computed: true},
			"endpoint_host":     schema.StringAttribute{Computed: true},
			"endpoint_port":     schema.Int64Attribute{Computed: true},
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

func (d *postgresqlClusterDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return exactlyOneOf("id", "name")
}

func (d *postgresqlClusterDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *postgresqlClusterDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config postgresqlClusterDataModel
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
	if db == nil {
		resp.Diagnostics.AddError("Cluster not found", "No PostgreSQL cluster matches in the workspace.")
		return
	}

	spec, st := db.GetSpec(), db.GetStatus()
	state := postgresqlClusterDataModel{
		ID:               types.StringValue(db.GetId()),
		WorkspaceID:      config.WorkspaceID,
		Name:             types.StringValue(db.GetName()),
		Version:          types.StringValue(spec.GetVersion()),
		Phase:            types.StringValue(st.GetPhase()),
		EndpointHost:     types.StringValue(st.GetEndpoint().GetHost()),
		EndpointPort:     types.Int64Value(int64(st.GetEndpoint().GetPort())),
		PublicEndpoints:  endpointsFromProto(st.GetPublicEndpoints()),
		PrivateEndpoints: endpointsFromProto(st.GetPrivateEndpoints()),
		StorageStatus:    storageStatusFromProto(st.GetStorage()),
		ReadyNodes:       types.Int64Value(int64(st.GetReadyNodes())),
		TotalNodes:       types.Int64Value(int64(st.GetTotalNodes())),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (d *postgresqlClusterDataSource) findByName(ctx context.Context, name string) (*serverv1.Database, error) {
	token := ""
	for {
		out, err := d.client.API.ListDatabases(ctx, &serverv1.ListDatabasesRequest{PageToken: token})
		if err != nil {
			return nil, err
		}
		for _, db := range out.GetDatabases() {
			if db.GetName() == name && db.GetSpec().GetEngine() == enginePostgreSQL {
				return db, nil
			}
		}
		token = out.GetNextPageToken()
		if token == "" {
			return nil, nil
		}
	}
}
