package provider

import (
	"context"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

func (f *fakeServer) GetBackupAutoExport(ctx context.Context, _ *serverv1.GetBackupAutoExportRequest) (*serverv1.GetBackupAutoExportResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "GetBackupAutoExport"); err != nil {
		return nil, err
	}
	if f.autoExport == nil {
		return &serverv1.GetBackupAutoExportResponse{}, nil
	}
	return &serverv1.GetBackupAutoExportResponse{AutoExport: f.autoExport}, nil
}

func (f *fakeServer) UpsertBackupAutoExport(ctx context.Context, req *serverv1.UpsertBackupAutoExportRequest) (*serverv1.UpsertBackupAutoExportResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "UpsertBackupAutoExport"); err != nil {
		return nil, err
	}
	f.lastAutoExport = req
	in := req.GetAutoExport()
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "auto_export is required")
	}
	if !uuidPattern.MatchString(in.GetIntegrationId()) {
		return nil, status.Error(codes.InvalidArgument, "id must be a UUID")
	}
	bucket := strings.TrimSpace(in.GetBucket())
	if !bucketPattern.MatchString(bucket) {
		return nil, status.Error(codes.InvalidArgument, "bucket is invalid")
	}
	scope := &serverv1.ClusterScope{Mode: "all", Ids: []string{}}
	if m := in.GetScope().GetMode(); m != "" {
		scope.Mode = m
	}
	switch scope.Mode {
	case "all":
	case "selected":
		for _, id := range in.GetScope().GetIds() {
			if !uuidPattern.MatchString(strings.ToLower(strings.TrimSpace(id))) {
				return nil, status.Error(codes.InvalidArgument, "invalid cluster id")
			}
			scope.Ids = append(scope.Ids, strings.ToLower(strings.TrimSpace(id)))
		}
		if len(scope.Ids) == 0 {
			return nil, status.Error(codes.InvalidArgument, "select at least one cluster")
		}
		slices.Sort(scope.Ids)
	default:
		return nil, status.Error(codes.InvalidArgument, "scope must be all or selected")
	}
	which := strings.TrimSpace(in.GetWhich())
	if !slices.Contains([]string{"all", "scheduled", "manual"}, which) {
		return nil, status.Error(codes.InvalidArgument, "which must be all, scheduled, or manual")
	}
	if !slices.ContainsFunc(f.cloudIntegrations, func(c *serverv1.CloudIntegration) bool { return c.GetId() == in.GetIntegrationId() }) {
		return nil, status.Error(codes.NotFound, "cloud integration not found")
	}
	stored := &serverv1.BackupAutoExport{
		IntegrationId:  in.GetIntegrationId(),
		Bucket:         bucket,
		Prefix:         strings.TrimSpace(in.GetPrefix()),
		Which:          which,
		Scope:          scope,
		Enabled:        in.GetEnabled(),
		LastError:      "AccessDenied",
		LastExportedAt: timestamppb.New(time.Unix(1_700_000_000, 0)),
	}
	if f.autoExport != nil {
		stored.LastError, stored.LastExportedAt = f.autoExport.GetLastError(), f.autoExport.GetLastExportedAt()
	}
	f.autoExport = stored
	returned := proto.Clone(stored).(*serverv1.BackupAutoExport)
	returned.LastError, returned.LastExportedAt = "", nil
	return &serverv1.UpsertBackupAutoExportResponse{AutoExport: returned}, nil
}
