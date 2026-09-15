package relay

import (
	"context"

	"google.golang.org/grpc/metadata"
)

const (
	orgHeader       = "x-relay-org-id"
	workspaceHeader = "x-relay-workspace-id"
)

func WithOrg(ctx context.Context, orgID string) context.Context {
	return withHeader(ctx, orgHeader, orgID)
}

func WithWorkspace(ctx context.Context, workspaceID string) context.Context {
	return withHeader(ctx, workspaceHeader, workspaceID)
}

func withHeader(ctx context.Context, key, value string) context.Context {
	if value == "" {
		return ctx
	}
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set(key, value)
	return metadata.NewOutgoingContext(ctx, md)
}
