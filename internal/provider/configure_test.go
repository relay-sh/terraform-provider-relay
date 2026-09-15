package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

func TestClientFromProviderData(t *testing.T) {
	var diags diag.Diagnostics
	if clientFromProviderData(nil, &diags) != nil || diags.HasError() {
		t.Error("nil provider data happens before configure and is not an error")
	}
	want := &client.Client{}
	if clientFromProviderData(want, &diags) != want || diags.HasError() {
		t.Error("a client must pass through")
	}
	if clientFromProviderData("nope", &diags) != nil || !diags.HasError() {
		t.Error("unexpected provider data must be reported")
	}
}
