package provider

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

func (f *fakeServer) CheckMigrationSource(ctx context.Context, req *serverv1.CheckMigrationSourceRequest) (*serverv1.CheckMigrationSourceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "CheckMigrationSource"); err != nil {
		return nil, err
	}
	f.lastCheck = proto.Clone(req).(*serverv1.CheckMigrationSourceRequest)
	id := f.newID("check")
	f.checkPolls[id] = 0
	src := req.GetSource()
	return &serverv1.CheckMigrationSourceResponse{SourceCheck: &serverv1.MigrationSourceCheck{
		Id:         id,
		DatabaseId: req.GetDatabaseId(),
		Source:     &serverv1.MigrationSource{Host: src.GetHost(), Port: src.GetPort(), Database: src.GetDatabase(), SslMode: src.GetSslMode()},
		Status:     &serverv1.MigrationSourceCheckStatus{Phase: serverv1.MigrationPhase_MIGRATION_PHASE_PENDING},
	}}, nil
}

func (f *fakeServer) GetMigrationSourceCheck(ctx context.Context, req *serverv1.GetMigrationSourceCheckRequest) (*serverv1.GetMigrationSourceCheckResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "GetMigrationSourceCheck"); err != nil {
		return nil, err
	}
	polls, ok := f.checkPolls[req.GetId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "source check not found")
	}
	f.checkPolls[req.GetId()]++
	check := &serverv1.MigrationSourceCheck{Id: req.GetId(), Status: &serverv1.MigrationSourceCheckStatus{Phase: serverv1.MigrationPhase_MIGRATION_PHASE_RUNNING}}
	if polls >= f.checkPendingFor {
		check.Status = proto.Clone(f.checkStatus).(*serverv1.MigrationSourceCheckStatus)
	}
	return &serverv1.GetMigrationSourceCheckResponse{SourceCheck: check}, nil
}

func completedCheck(results ...*serverv1.MigrationSourceCheckResult) *serverv1.MigrationSourceCheckStatus {
	return &serverv1.MigrationSourceCheckStatus{
		Phase:     serverv1.MigrationPhase_MIGRATION_PHASE_COMPLETED,
		ExpiresAt: timestamppb.New(fakeNow.Add(time.Hour)),
		Results:   results,
		Modes:     []serverv1.MigrationMode{serverv1.MigrationMode_MIGRATION_MODE_ONLINE, serverv1.MigrationMode_MIGRATION_MODE_OFFLINE},
		Facts: &serverv1.MigrationSourceCheckFacts{
			ServerVersion: "16.4", Encoding: "UTF8", DatabaseSizeBytes: 53687091200, Owner: "app",
			MaxSlotWalKeepSize: "-1", WalBytesPerSecond: 1048576, LargestRowBytes: 4096, Provider: "aws-rds",
		},
	}
}
