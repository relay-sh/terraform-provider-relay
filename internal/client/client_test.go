package client

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"
)

func TestScopes(t *testing.T) {
	c := &Client{OrgID: "org-1"}

	md, _ := metadata.FromOutgoingContext(c.Org(context.Background()))
	if got := md.Get("x-relay-org-id"); len(got) != 1 || got[0] != "org-1" {
		t.Errorf("org header = %v", got)
	}
	if got := md.Get("x-relay-workspace-id"); len(got) != 0 {
		t.Errorf("an org-scoped call must not carry a workspace, got %v", got)
	}

	md, _ = metadata.FromOutgoingContext(c.Workspace(context.Background(), "ws-1"))
	if md.Get("x-relay-org-id")[0] != "org-1" || md.Get("x-relay-workspace-id")[0] != "ws-1" {
		t.Errorf("workspace scope = %v", md)
	}
}
