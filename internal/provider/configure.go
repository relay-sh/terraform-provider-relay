package provider

import (
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

func clientFromProviderData(data any, diags *diag.Diagnostics) *client.Client {
	if data == nil {
		return nil
	}
	c, ok := data.(*client.Client)
	if !ok {
		diags.AddError("Unexpected provider data", fmt.Sprintf("Expected *client.Client, got %T.", data))
		return nil
	}
	return c
}

func exactlyOneOf(attrs ...string) []datasource.ConfigValidator {
	exprs := make([]path.Expression, 0, len(attrs))
	for _, a := range attrs {
		exprs = append(exprs, path.MatchRoot(a))
	}
	return []datasource.ConfigValidator{datasourcevalidator.ExactlyOneOf(exprs...)}
}
