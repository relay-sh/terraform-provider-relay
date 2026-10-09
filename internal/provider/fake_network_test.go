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

func (f *fakeServer) serviceIn(ctx context.Context, id string) (*serverv1.PrivateEndpointService, error) {
	svc, ok := f.peServices[id]
	if !ok || svc.GetCreatedBy() != workspaceOf(ctx) {
		return nil, status.Error(codes.NotFound, "private endpoint service not found")
	}
	return svc, nil
}

func (f *fakeServer) CreatePrivateEndpointService(ctx context.Context, req *serverv1.CreatePrivateEndpointServiceRequest) (*serverv1.CreatePrivateEndpointServiceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "CreatePrivateEndpointService"); err != nil {
		return nil, err
	}
	served := slices.ContainsFunc(f.peRegions, func(r *serverv1.PrivateEndpointRegion) bool {
		return r.GetProvider() == req.GetProvider() && r.GetRegion() == req.GetRegion()
	})
	if !served {
		return nil, status.Errorf(codes.InvalidArgument, "no private endpoint service is available in %s/%s", req.GetProvider(), req.GetRegion())
	}
	ws := workspaceOf(ctx)
	for _, s := range f.peServices {
		if s.GetCreatedBy() == ws && s.GetProvider() == req.GetProvider() && s.GetRegion() == req.GetRegion() {
			return nil, status.Error(codes.AlreadyExists, "workspace already has a service there")
		}
	}
	svc := &serverv1.PrivateEndpointService{
		Id:                  f.newID("pes"),
		Provider:            req.GetProvider(),
		Region:              req.GetRegion(),
		Status:              endpointStatusAvailable,
		EndpointServiceName: "com.amazonaws.vpce." + req.GetRegion() + ".vpce-svc-0123456789abcdef0",
		CreatedBy:           ws,
		CreatedAt:           timestamppb.New(fakeEpoch),
	}
	if f.initiatingFor > 0 {
		svc.Status = "INITIATING"
		f.svcCountdown[svc.Id] = f.initiatingFor
	}
	f.peServices[svc.Id] = svc
	return &serverv1.CreatePrivateEndpointServiceResponse{Service: svc}, nil
}

func (f *fakeServer) GetPrivateEndpointService(ctx context.Context, req *serverv1.GetPrivateEndpointServiceRequest) (*serverv1.GetPrivateEndpointServiceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "GetPrivateEndpointService"); err != nil {
		return nil, err
	}
	svc, err := f.serviceIn(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	if f.svcCountdown[svc.Id] > 0 {
		f.svcCountdown[svc.Id]--
		if f.svcCountdown[svc.Id] == 0 {
			svc.Status = endpointStatusAvailable
			if f.failService != "" {
				svc.Status, svc.ErrorMessage = endpointStatusFailed, f.failService
			}
		}
	}
	return &serverv1.GetPrivateEndpointServiceResponse{Service: svc}, nil
}

func (f *fakeServer) ListPrivateEndpointServices(ctx context.Context, _ *serverv1.ListPrivateEndpointServicesRequest) (*serverv1.ListPrivateEndpointServicesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "ListPrivateEndpointServices"); err != nil {
		return nil, err
	}
	out := &serverv1.ListPrivateEndpointServicesResponse{}
	for _, s := range f.peServices {
		if s.GetCreatedBy() == workspaceOf(ctx) {
			out.Services = append(out.Services, s)
		}
	}
	return out, nil
}

func (f *fakeServer) DeletePrivateEndpointService(ctx context.Context, req *serverv1.DeletePrivateEndpointServiceRequest) (*serverv1.DeletePrivateEndpointServiceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "DeletePrivateEndpointService"); err != nil {
		return nil, err
	}
	svc, err := f.serviceIn(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	if f.serviceBlocked > 0 || len(svc.GetEndpoints()) > 0 {
		f.serviceBlocked--
		return nil, status.Error(codes.FailedPrecondition, "service still has endpoints")
	}
	delete(f.peServices, svc.Id)
	return &serverv1.DeletePrivateEndpointServiceResponse{}, nil
}

func (f *fakeServer) endpointIn(ctx context.Context, serviceID, id string) (*serverv1.PrivateEndpointService, int, error) {
	svc, err := f.serviceIn(ctx, serviceID)
	if err != nil {
		return nil, 0, err
	}
	i := slices.IndexFunc(svc.Endpoints, func(e *serverv1.PrivateEndpoint) bool { return e.GetId() == id })
	if i < 0 {
		return nil, 0, status.Error(codes.NotFound, "private endpoint not found")
	}
	return svc, i, nil
}

func (f *fakeServer) CreatePrivateEndpoint(ctx context.Context, req *serverv1.CreatePrivateEndpointRequest) (*serverv1.CreatePrivateEndpointResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "CreatePrivateEndpoint"); err != nil {
		return nil, err
	}
	svc, err := f.serviceIn(ctx, req.GetServiceId())
	if err != nil {
		return nil, err
	}
	for _, e := range svc.Endpoints {
		if e.GetInterfaceEndpointId() == req.GetInterfaceEndpointId() {
			return nil, status.Error(codes.AlreadyExists, "endpoint is already registered on this service")
		}
	}
	ep := &serverv1.PrivateEndpoint{
		Id:                  f.newID("pe"),
		ServiceId:           svc.Id,
		Provider:            svc.Provider,
		Region:              svc.Region,
		Index:               int32(f.nextID),
		InterfaceEndpointId: req.GetInterfaceEndpointId(),
		ConnectionStatus:    "PENDING",
		CreatedAt:           timestamppb.New(fakeEpoch),
	}
	f.epCountdown[ep.Id] = f.pendingFor
	svc.Endpoints = append(svc.Endpoints, ep)
	return &serverv1.CreatePrivateEndpointResponse{Endpoint: ep}, nil
}

func (f *fakeServer) GetPrivateEndpoint(ctx context.Context, req *serverv1.GetPrivateEndpointRequest) (*serverv1.GetPrivateEndpointResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "GetPrivateEndpoint"); err != nil {
		return nil, err
	}
	svc, i, err := f.endpointIn(ctx, req.GetServiceId(), req.GetId())
	if err != nil {
		return nil, err
	}
	ep := svc.Endpoints[i]
	if f.epCountdown[ep.Id] > 0 {
		f.epCountdown[ep.Id]--
		return &serverv1.GetPrivateEndpointResponse{Endpoint: ep}, nil
	}
	switch {
	case ep.GetDeleteRequested():
		svc.Endpoints = slices.Delete(svc.Endpoints, i, i+1)
		return nil, status.Error(codes.NotFound, "private endpoint not found")
	case f.rejectEndpoint != "":
		ep.ConnectionStatus, ep.ErrorMessage = endpointStatusRejected, f.rejectEndpoint
	default:
		ep.ConnectionStatus, ep.DnsName = endpointStatusAvailable, ep.GetInterfaceEndpointId()+".vpce-svc.us-east-1.vpce.amazonaws.com"
	}
	return &serverv1.GetPrivateEndpointResponse{Endpoint: ep}, nil
}

func (f *fakeServer) DeletePrivateEndpoint(ctx context.Context, req *serverv1.DeletePrivateEndpointRequest) (*serverv1.DeletePrivateEndpointResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "DeletePrivateEndpoint"); err != nil {
		return nil, err
	}
	svc, i, err := f.endpointIn(ctx, req.GetServiceId(), req.GetId())
	if err != nil {
		return nil, err
	}
	ep := svc.Endpoints[i]
	ep.DeleteRequested, ep.ConnectionStatus = true, "DELETING"
	f.epCountdown[ep.Id] = f.deletingFor
	return &serverv1.DeletePrivateEndpointResponse{}, nil
}

func (f *fakeServer) ListPrivateEndpointRegions(ctx context.Context, _ *serverv1.ListPrivateEndpointRegionsRequest) (*serverv1.ListPrivateEndpointRegionsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "ListPrivateEndpointRegions"); err != nil {
		return nil, err
	}
	return &serverv1.ListPrivateEndpointRegionsResponse{Regions: f.peRegions}, nil
}
