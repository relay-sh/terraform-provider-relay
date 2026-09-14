package client

import (
	"context"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

type Client struct {
	API   serverv1.ServerServiceClient
	OrgID string
}

func (c *Client) Org(ctx context.Context) context.Context {
	return relay.WithOrg(ctx, c.OrgID)
}

func (c *Client) Workspace(ctx context.Context, workspaceID string) context.Context {
	return relay.WithWorkspace(c.Org(ctx), workspaceID)
}
