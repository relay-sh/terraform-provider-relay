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

type databaseDataSource struct {
	client *client.Client
}

type databaseDataModel struct {
	ID               types.String `tfsdk:"id"`
	WorkspaceID      types.String `tfsdk:"workspace_id"`
	Name             types.String `tfsdk:"name"`
	Engine           types.String `tfsdk:"engine"`
	Version          types.String `tfsdk:"version"`
	Topology         types.String `tfsdk:"topology"`
	BackupsEnabled   types.Bool   `tfsdk:"backups_enabled"`
	Phase            types.String `tfsdk:"phase"`
	EndpointHost     types.String `tfsdk:"endpoint_host"`
	EndpointPort     types.Int64  `tfsdk:"endpoint_port"`
	PublicEndpoints  types.List   `tfsdk:"public_endpoints"`
	PrivateEndpoints types.List   `tfsdk:"private_endpoints"`
}

func newDatabaseDataSource() datasource.DataSource {
	return &databaseDataSource{}
}

func (d *databaseDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_database"
}

func (d *databaseDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	endpoint := schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
		"role": schema.StringAttribute{Computed: true},
		"host": schema.StringAttribute{Computed: true},
		"port": schema.Int64Attribute{Computed: true},
	}}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads an existing managed database in a workspace, by ID or by name.",
		Attributes: map[string]schema.Attribute{
			"workspace_id":      schema.StringAttribute{Required: true, MarkdownDescription: "Workspace to look in."},
			"id":                schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Database ID."},
			"name":              schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Database name."},
			"engine":            schema.StringAttribute{Computed: true},
			"version":           schema.StringAttribute{Computed: true},
			"topology":          schema.StringAttribute{Computed: true},
			"backups_enabled":   schema.BoolAttribute{Computed: true},
			"phase":             schema.StringAttribute{Computed: true},
			"endpoint_host":     schema.StringAttribute{Computed: true},
			"endpoint_port":     schema.Int64Attribute{Computed: true},
			"public_endpoints":  schema.ListNestedAttribute{Computed: true, NestedObject: endpoint},
			"private_endpoints": schema.ListNestedAttribute{Computed: true, NestedObject: endpoint},
		},
	}
}

func (d *databaseDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return exactlyOneOf("id", "name")
}

func (d *databaseDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *databaseDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config databaseDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	scoped := d.client.Workspace(ctx, config.WorkspaceID.ValueString())

	var db *serverv1.Database
	if !config.ID.IsNull() {
		out, err := d.client.API.GetDatabase(scoped, &serverv1.GetDatabaseRequest{Id: config.ID.ValueString()})
		if err != nil && !relay.IsNotFound(err) {
			resp.Diagnostics.AddError("Cannot read database", err.Error())
			return
		}
		db = out.GetDatabase()
	} else {
		found, err := d.findByName(scoped, config.Name.ValueString())
		if err != nil {
			resp.Diagnostics.AddError("Cannot list databases", err.Error())
			return
		}
		db = found
	}
	if db == nil {
		resp.Diagnostics.AddError("Database not found", "No database matches in the workspace.")
		return
	}

	spec, st := db.GetSpec(), db.GetStatus()
	state := databaseDataModel{
		ID:               types.StringValue(db.GetId()),
		WorkspaceID:      config.WorkspaceID,
		Name:             types.StringValue(db.GetName()),
		Engine:           types.StringValue(spec.GetEngine()),
		Version:          types.StringValue(spec.GetVersion()),
		Topology:         types.StringValue(spec.GetTopology()),
		BackupsEnabled:   types.BoolValue(spec.GetBackupsEnabled()),
		Phase:            types.StringValue(st.GetPhase()),
		EndpointHost:     types.StringValue(st.GetEndpoint().GetHost()),
		EndpointPort:     types.Int64Value(int64(st.GetEndpoint().GetPort())),
		PublicEndpoints:  endpointsFromProto(st.GetPublicEndpoints()),
		PrivateEndpoints: endpointsFromProto(st.GetPrivateEndpoints()),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (d *databaseDataSource) findByName(ctx context.Context, name string) (*serverv1.Database, error) {
	token := ""
	for {
		out, err := d.client.API.ListDatabases(ctx, &serverv1.ListDatabasesRequest{PageToken: token})
		if err != nil {
			return nil, err
		}
		for _, db := range out.GetDatabases() {
			if db.GetName() == name {
				return db, nil
			}
		}
		token = out.GetNextPageToken()
		if token == "" {
			return nil, nil
		}
	}
}
