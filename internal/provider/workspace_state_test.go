package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

func TestWorkspaceStateWithIsDefaultStillLoads(t *testing.T) {
	server, err := providerserver.NewProtocol6WithError(New("test")())()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{}); err != nil {
		t.Fatal(err)
	}
	resp, err := server.UpgradeResourceState(ctx, &tfprotov6.UpgradeResourceStateRequest{
		TypeName: "relay_workspace",
		RawState: &tfprotov6.RawState{JSON: []byte(`{"id":"ws-1","name":"prod","description":"","slug":"prod","is_default":true,"managed_by":"terraform"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range resp.Diagnostics {
		if d.Severity == tfprotov6.DiagnosticSeverityError {
			t.Fatalf("state written by a provider with is_default: %s: %s", d.Summary, d.Detail)
		}
	}
	if resp.UpgradedState == nil {
		t.Fatal("no upgraded state")
	}
}
