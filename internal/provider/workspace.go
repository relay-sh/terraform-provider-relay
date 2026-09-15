package provider

import (
	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

const managedBy = "terraform"

type workspaceModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Slug        types.String `tfsdk:"slug"`
	IsDefault   types.Bool   `tfsdk:"is_default"`
	ManagedBy   types.String `tfsdk:"managed_by"`
}

func workspaceFromProto(w *serverv1.Workspace) workspaceModel {
	return workspaceModel{
		ID:          types.StringValue(w.GetId()),
		Name:        types.StringValue(w.GetName()),
		Description: types.StringValue(w.GetDescription()),
		Slug:        types.StringValue(w.GetSlug()),
		IsDefault:   types.BoolValue(w.GetIsDefault()),
		ManagedBy:   types.StringValue(w.GetManagedBy()),
	}
}
