package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

func deletionProtectionAttribute(what string) schema.BoolAttribute {
	return schema.BoolAttribute{
		Optional: true, Computed: true, Default: booldefault.StaticBool(false),
		MarkdownDescription: "While `true`, the platform refuses to delete the " + what + ", and `terraform destroy` or a replacement fails before deleting it. " +
			"Turning it on needs deletion protection enabled for the organization. Turning it off needs the `org.deletion_protection.disable` permission, which the owner and admin roles hold.",
	}
}

func refuseProtectedDelete(protected types.Bool, diags *diag.Diagnostics) bool {
	if !protected.ValueBool() {
		return false
	}
	diags.AddAttributeError(path.Root("deletion_protection"), "Deletion protection is on",
		"deletion_protection is true; set deletion_protection = false and apply before destroying.")
	return true
}

func addDeletionProtectionError(diags *diag.Diagnostics, summary string, err error, disabling bool) {
	if disabling && status.Code(err) == codes.PermissionDenied {
		diags.AddAttributeError(path.Root("deletion_protection"), "Cannot turn deletion protection off",
			"Turning deletion protection off needs the org.deletion_protection.disable permission, which the owner and admin roles hold. "+status.Convert(err).Message())
		return
	}
	diags.AddError(summary, err.Error())
}

func setClusterDeletionProtection(ctx context.Context, c *client.Client, id string, want, current types.Bool, diags *diag.Diagnostics) (*serverv1.Database, bool) {
	if want.Equal(current) {
		return nil, true
	}
	out, err := c.API.SetDeletionProtection(ctx, &serverv1.SetDeletionProtectionRequest{Id: id, Enabled: want.ValueBool()})
	if err != nil {
		addDeletionProtectionError(diags, "Cannot change deletion protection", err, !want.ValueBool())
		return nil, false
	}
	return out.GetDatabase(), true
}
