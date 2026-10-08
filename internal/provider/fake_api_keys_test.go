package provider

import (
	"context"
	"net/netip"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

var fakeNow = time.Unix(1_800_000_000, 0).UTC()

func normalizeCIDRs(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if p, err := netip.ParsePrefix(v); err == nil {
			out = append(out, p.Masked().String())
			continue
		}
		a, err := netip.ParseAddr(v)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "%q is not an IP address or CIDR", v)
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()).String())
	}
	return out, nil
}

func (f *fakeServer) keyLimits(cidrs []string, expires *timestamppb.Timestamp) ([]string, *timestamppb.Timestamp) {
	if f.callerKey == nil {
		return cidrs, expires
	}
	if len(cidrs) == 0 {
		cidrs = f.callerKey.GetAllowedCidrs()
	}
	if expires == nil {
		expires = f.callerKey.GetExpiresAt()
	}
	return cidrs, expires
}

func expiryFrom(seconds int64) *timestamppb.Timestamp {
	if seconds == 0 {
		return nil
	}
	return timestamppb.New(fakeNow.Add(time.Duration(seconds) * time.Second))
}

func (f *fakeServer) CreateAPIKey(ctx context.Context, req *serverv1.CreateAPIKeyRequest) (*serverv1.CreateAPIKeyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "CreateAPIKey"); err != nil {
		return nil, err
	}
	f.lastKeyCreate = req
	if req.GetWorkspaceId() != "" && f.workspaces[req.GetWorkspaceId()] == nil {
		return nil, status.Error(codes.NotFound, "workspace not found")
	}
	if req.GetRoleSlug() == "owner" {
		return nil, status.Error(codes.PermissionDenied, "API keys cannot hold an owner-level role")
	}
	cidrs, err := normalizeCIDRs(req.GetAllowedCidrs())
	if err != nil {
		return nil, err
	}
	cidrs, expires := f.keyLimits(cidrs, expiryFrom(req.GetExpiresInSeconds()))
	k := &serverv1.APIKey{
		Id:           f.newID("key"),
		Name:         strings.TrimSpace(req.GetName()),
		WorkspaceId:  req.GetWorkspaceId(),
		RoleSlug:     req.GetRoleSlug(),
		AllowedCidrs: cidrs,
		CreatedAt:    timestamppb.New(fakeNow),
		ExpiresAt:    expires,
		ParentKeyId:  f.callerKey.GetId(),
	}
	f.apiKeys[k.Id] = k
	return &serverv1.CreateAPIKeyResponse{Key: k, Plaintext: "rly_key_" + k.Id}, nil
}

func (f *fakeServer) ListAPIKeys(ctx context.Context, req *serverv1.ListAPIKeysRequest) (*serverv1.ListAPIKeysResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "ListAPIKeys"); err != nil {
		return nil, err
	}
	if req.GetWorkspaceId() != "" && f.workspaces[req.GetWorkspaceId()] == nil {
		return nil, status.Error(codes.NotFound, "workspace not found")
	}
	var out []*serverv1.APIKey
	for _, k := range f.apiKeys {
		if k.GetWorkspaceId() == req.GetWorkspaceId() {
			out = append(out, k)
		}
	}
	return &serverv1.ListAPIKeysResponse{Keys: out}, nil
}

func (f *fakeServer) UpdateAPIKey(ctx context.Context, req *serverv1.UpdateAPIKeyRequest) (*serverv1.UpdateAPIKeyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "UpdateAPIKey"); err != nil {
		return nil, err
	}
	f.lastKeyUpdate = req
	k := f.apiKeys[req.GetId()]
	if k == nil {
		return nil, status.Error(codes.NotFound, "API key not found")
	}
	cidrs, err := normalizeCIDRs(req.GetAllowedCidrs())
	if err != nil {
		return nil, err
	}
	expires := k.GetExpiresAt()
	if req.ExpiresInSeconds != nil {
		_, expires = f.keyLimits(nil, expiryFrom(req.GetExpiresInSeconds()))
	}
	k.Name = req.GetName()
	k.AllowedCidrs, _ = f.keyLimits(cidrs, nil)
	k.ExpiresAt = expires
	if req.RoleSlug != nil {
		k.RoleSlug = req.GetRoleSlug()
	}
	return &serverv1.UpdateAPIKeyResponse{Key: k}, nil
}

func (f *fakeServer) RevokeAPIKey(ctx context.Context, req *serverv1.RevokeAPIKeyRequest) (*serverv1.RevokeAPIKeyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "RevokeAPIKey"); err != nil {
		return nil, err
	}
	_, ok := f.apiKeys[req.GetId()]
	delete(f.apiKeys, req.GetId())
	return &serverv1.RevokeAPIKeyResponse{Revoked: ok}, nil
}
