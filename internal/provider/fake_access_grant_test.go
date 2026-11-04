package provider

import (
	"context"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

func (f *fakeServer) ListAccessGrants(ctx context.Context, req *serverv1.ListAccessGrantsRequest) (*serverv1.ListAccessGrantsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "ListAccessGrants"); err != nil {
		return nil, err
	}
	filtered := req.GetTargetType() != "" && req.GetTargetName() != ""
	if filtered && f.grantTargetGone {
		return nil, status.Errorf(codes.NotFound, "connection %q not found", req.GetTargetName())
	}
	var out []*serverv1.AccessGrant
	for _, g := range f.grants[workspaceOf(ctx)] {
		if !filtered || (g.GetTargetType() == req.GetTargetType() && g.GetTargetName() == req.GetTargetName()) {
			out = append(out, g)
		}
	}
	return &serverv1.ListAccessGrantsResponse{Grants: out}, nil
}

func (f *fakeServer) CreateAccessGrant(ctx context.Context, req *serverv1.CreateAccessGrantRequest) (*serverv1.CreateAccessGrantResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "CreateAccessGrant"); err != nil {
		return nil, err
	}
	f.lastGrant = req
	if req.GetExpiresAt() != nil && !req.GetExpiresAt().AsTime().After(time.Now()) {
		return nil, status.Error(codes.InvalidArgument, "expires_at must be in the future")
	}
	ws := workspaceOf(ctx)
	grantee := req.GetGranteeName()
	if req.GetGranteeType() == "user" {
		grantee = strings.ToLower(grantee)
	}
	for _, g := range f.grants[ws] {
		if g.GetGranteeType() == req.GetGranteeType() && g.GetGranteeName() == grantee && g.GetTargetType() == req.GetTargetType() && g.GetTargetName() == req.GetTargetName() {
			return nil, status.Error(codes.AlreadyExists, "access grant already exists")
		}
	}
	g := &serverv1.AccessGrant{
		Id:          f.newID("grant"),
		GranteeType: req.GetGranteeType(),
		GranteeName: grantee,
		TargetType:  req.GetTargetType(),
		TargetName:  req.GetTargetName(),
		GrantedBy:   "admin@example.com",
		CreatedAt:   timestamppb.New(fakeEpoch),
		ExpiresAt:   req.GetExpiresAt(),
	}
	f.grants[ws] = append(f.grants[ws], g)
	return &serverv1.CreateAccessGrantResponse{Grant: g}, nil
}

func (f *fakeServer) DeleteAccessGrant(ctx context.Context, req *serverv1.DeleteAccessGrantRequest) (*serverv1.DeleteAccessGrantResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "DeleteAccessGrant"); err != nil {
		return nil, err
	}
	ws := workspaceOf(ctx)
	i := slices.IndexFunc(f.grants[ws], func(g *serverv1.AccessGrant) bool { return g.GetId() == req.GetId() })
	if i < 0 {
		return nil, status.Errorf(codes.NotFound, "access grant %q not found", req.GetId())
	}
	f.grants[ws] = slices.Delete(f.grants[ws], i, i+1)
	return &serverv1.DeleteAccessGrantResponse{}, nil
}
