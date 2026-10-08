package provider

import (
	"context"
	"net/netip"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

var fakeEpoch = time.Unix(1_700_000_000, 0)

func canonical(cidr string) (string, error) {
	if a, err := netip.ParseAddr(cidr); err == nil {
		return netip.PrefixFrom(a, a.BitLen()).String(), nil
	}
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, "%q is not a valid CIDR", cidr)
	}
	return p.Masked().String(), nil
}

func (f *fakeServer) startSync(ws string) {
	f.syncCountdown[ws] = f.syncingFor
}

func (f *fakeServer) ListIPAllowlist(ctx context.Context, _ *serverv1.ListIPAllowlistRequest) (*serverv1.ListIPAllowlistResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "ListIPAllowlist"); err != nil {
		return nil, err
	}
	ws := workspaceOf(ctx)
	syncing := f.syncCountdown[ws] > 0
	if syncing {
		f.syncCountdown[ws]--
	}
	return &serverv1.ListIPAllowlistResponse{Entries: f.allowlist[ws], Syncing: syncing}, nil
}

func (f *fakeServer) AddIPAllowlistEntry(ctx context.Context, req *serverv1.AddIPAllowlistEntryRequest) (*serverv1.AddIPAllowlistEntryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "AddIPAllowlistEntry"); err != nil {
		return nil, err
	}
	cidr, err := canonical(req.GetCidr())
	if err != nil {
		return nil, err
	}
	ws := workspaceOf(ctx)
	for _, e := range f.allowlist[ws] {
		if e.GetCidr() == cidr {
			return nil, status.Errorf(codes.AlreadyExists, "cidr %s is already on the allowlist", cidr)
		}
	}
	e := &serverv1.IPAllowlistEntry{Id: f.newID("ip"), Cidr: cidr, Description: req.GetDescription(), CreatedBy: "key-1", CreatedAt: timestamppb.New(fakeEpoch)}
	if req.GetTtlSeconds() > 0 {
		e.ExpiresAt = timestamppb.New(fakeEpoch.Add(time.Hour))
	}
	f.allowlist[ws] = append(f.allowlist[ws], e)
	f.startSync(ws)
	return &serverv1.AddIPAllowlistEntryResponse{Entry: e}, nil
}

func (f *fakeServer) entryIndex(ws, id string) int {
	return slices.IndexFunc(f.allowlist[ws], func(e *serverv1.IPAllowlistEntry) bool { return e.GetId() == id })
}

func (f *fakeServer) UpdateIPAllowlistEntry(ctx context.Context, req *serverv1.UpdateIPAllowlistEntryRequest) (*serverv1.UpdateIPAllowlistEntryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "UpdateIPAllowlistEntry"); err != nil {
		return nil, err
	}
	f.lastAllowUpdate = req
	ws := workspaceOf(ctx)
	i := f.entryIndex(ws, req.GetId())
	if i < 0 {
		return nil, status.Error(codes.NotFound, "allowlist entry not found")
	}
	e := f.allowlist[ws][i]
	if req.Cidr != nil {
		cidr, err := canonical(req.GetCidr())
		if err != nil {
			return nil, err
		}
		e.Cidr = cidr
	}
	if req.Description != nil {
		e.Description = req.GetDescription()
	}
	f.startSync(ws)
	return &serverv1.UpdateIPAllowlistEntryResponse{Entry: e}, nil
}

func (f *fakeServer) RemoveIPAllowlistEntry(ctx context.Context, req *serverv1.RemoveIPAllowlistEntryRequest) (*serverv1.RemoveIPAllowlistEntryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "RemoveIPAllowlistEntry"); err != nil {
		return nil, err
	}
	ws := workspaceOf(ctx)
	i := f.entryIndex(ws, req.GetId())
	if i < 0 {
		return nil, status.Error(codes.NotFound, "allowlist entry not found")
	}
	f.allowlist[ws] = slices.Delete(f.allowlist[ws], i, i+1)
	f.startSync(ws)
	return &serverv1.RemoveIPAllowlistEntryResponse{}, nil
}
