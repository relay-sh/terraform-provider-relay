package provider

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

var platformDefaultTiers = []*serverv1.PolicyTier{
	{Frequency: "hourly", Interval: 6, RetentionValue: 28, RetentionUnit: "days", Enabled: true},
	{Frequency: "daily", Interval: 1, RetentionValue: 7, RetentionUnit: "days", Enabled: true},
	{Frequency: "weekly", Interval: 1, RetentionValue: 4, RetentionUnit: "weeks", Enabled: true},
	{Frequency: "monthly", Interval: 1, RetentionValue: 12, RetentionUnit: "months", Enabled: true},
}

func (f *fakeServer) scheduledDatabase(ctx context.Context, id string) (*serverv1.Database, error) {
	db, ok := f.databases[id]
	if !ok || db.GetWorkspaceId() != workspaceOf(ctx) {
		return nil, status.Errorf(codes.NotFound, "database %s not found", id)
	}
	if e := db.GetSpec().GetEngine(); e != enginePostgreSQL && e != engineValkey {
		return nil, status.Errorf(codes.FailedPrecondition, "engine %q has no cluster-keyed backup schedule", e)
	}
	if !db.GetSpec().GetBackupsEnabled() {
		return nil, status.Errorf(codes.FailedPrecondition, "enable backups on database %s before setting its schedule", id)
	}
	return db, nil
}

func (f *fakeServer) GetBackupSchedule(ctx context.Context, req *serverv1.GetBackupScheduleRequest) (*serverv1.GetBackupScheduleResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "GetBackupSchedule"); err != nil {
		return nil, err
	}
	db, err := f.scheduledDatabase(ctx, req.GetDatabaseId())
	if err != nil {
		return nil, status.Error(codes.NotFound, status.Convert(err).Message())
	}
	return &serverv1.GetBackupScheduleResponse{BackupSchedule: synthesizedSchedule(db, f.schedules[db.GetId()])}, nil
}

func (f *fakeServer) UpsertBackupSchedule(ctx context.Context, req *serverv1.UpsertBackupScheduleRequest) (*serverv1.UpsertBackupScheduleResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "UpsertBackupSchedule"); err != nil {
		return nil, err
	}
	f.lastScheduleSpec = req.GetSpec()
	if req.GetSpec() == nil {
		return nil, status.Error(codes.InvalidArgument, "spec is required")
	}
	if _, ok := f.databases[req.GetDatabaseId()]; !ok {
		return nil, status.Error(codes.NotFound, "database not found")
	}
	if requested := requestedRetentionDays(req.GetSpec()); f.planRetentionDays > 0 && requested > f.planRetentionDays {
		_ = grpc.SetTrailer(ctx, metadata.Pairs("x-relay-error-code", "PLAN_LIMIT_REACHED"))
		return nil, status.Errorf(codes.FailedPrecondition, "free plan allows at most %d days of backup retention (asked for %d days) — upgrade for a longer window", f.planRetentionDays, requested)
	}
	db, err := f.scheduledDatabase(ctx, req.GetDatabaseId())
	if err != nil {
		return nil, err
	}
	if err := validateSchedule(db.GetSpec().GetEngine(), req.GetSpec()); err != nil {
		return nil, status.Error(codes.InvalidArgument, "validate spec: "+err.Error())
	}
	stored := proto.Clone(req.GetSpec()).(*serverv1.BackupScheduleSpec)
	f.schedules[db.GetId()] = stored
	return &serverv1.UpsertBackupScheduleResponse{BackupSchedule: synthesizedSchedule(db, stored)}, nil
}

func (f *fakeServer) ResetBackupSchedule(ctx context.Context, req *serverv1.ResetBackupScheduleRequest) (*serverv1.ResetBackupScheduleResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "ResetBackupSchedule"); err != nil {
		return nil, err
	}
	if _, ok := f.databases[req.GetDatabaseId()]; !ok {
		return nil, status.Error(codes.NotFound, "database not found")
	}
	db, err := f.scheduledDatabase(ctx, req.GetDatabaseId())
	if err != nil {
		return nil, err
	}
	delete(f.schedules, db.GetId())
	return &serverv1.ResetBackupScheduleResponse{BackupSchedule: synthesizedSchedule(db, nil)}, nil
}

func requestedRetentionDays(spec *serverv1.BackupScheduleSpec) int64 {
	longest := max(int64(spec.GetRetention().GetMaxDays()), int64(spec.GetRetention().GetMaxBackups())*7, int64(spec.GetPitrWindowDays()))
	for _, t := range spec.GetPolicyTiers() {
		if !t.GetEnabled() {
			continue
		}
		days := int64(t.GetRetentionValue())
		switch t.GetRetentionUnit() {
		case "weeks":
			days *= 7
		case "months":
			days *= 30
		}
		longest = max(longest, days)
	}
	return longest
}

func validateSchedule(engine string, spec *serverv1.BackupScheduleSpec) error {
	switch t := spec.GetSnapshotTime(); {
	case spec.GetVolumeSnapshotClass() != "":
		return fmt.Errorf("volume_snapshot_class is not supported for %s schedules", engine)
	case spec.GetPaused():
		return fmt.Errorf("paused is not supported for %s schedules", engine)
	case spec.GetRetention().GetMaxDays() != 0:
		return fmt.Errorf("retention.max_days is not supported for %s schedules", engine)
	case spec.GetRetention() != nil && spec.GetRetention().GetMaxBackups() < 1:
		return fmt.Errorf("retention.max_backups must be at least 1")
	case engine == engineValkey && spec.GetPitrWindowDays() != 0:
		return fmt.Errorf("pitr_window_days is not supported for Valkey; restores are snapshot-only")
	case t != nil && (t.GetHour() < 0 || t.GetHour() > 23 || t.GetMinute() < 0 || t.GetMinute() > 59):
		return fmt.Errorf("snapshot_time must be a UTC wall-clock time (hour 0-23, minute 0-59)")
	}
	seen := map[string]bool{}
	for i, t := range spec.GetPolicyTiers() {
		rule, ok := tierRules[t.GetFrequency()]
		if !ok {
			return fmt.Errorf("policy_tiers[%d]: frequency %q is not supported for %s (hourly, daily, weekly, monthly)", i, t.GetFrequency(), engine)
		}
		if seen[t.GetFrequency()] {
			return fmt.Errorf("policy_tiers[%d]: more than one %s tier", i, t.GetFrequency())
		}
		seen[t.GetFrequency()] = true
		if !t.GetEnabled() {
			continue
		}
		switch {
		case !rule.interval(int64(t.GetInterval())):
			return fmt.Errorf("policy_tiers[%d]: %s interval must be %s, got %d", i, t.GetFrequency(), rule.hint, t.GetInterval())
		case t.GetRetentionValue() < 1:
			return fmt.Errorf("policy_tiers[%d]: %s retention must be at least 1", i, t.GetFrequency())
		case t.GetRetentionUnit() != "" && t.GetRetentionUnit() != rule.unit:
			return fmt.Errorf("policy_tiers[%d]: %s retention is counted in %s, got %q", i, t.GetFrequency(), rule.unit, t.GetRetentionUnit())
		}
	}
	return nil
}

func synthesizedSchedule(db *serverv1.Database, stored *serverv1.BackupScheduleSpec) *serverv1.BackupSchedule {
	spec := &serverv1.BackupScheduleSpec{
		Engine:       db.GetSpec().GetEngine(),
		ClusterRef:   db.GetName(),
		SnapshotTime: &serverv1.SnapshotTime{Hour: 2},
	}
	retained := int32(4)
	switch {
	case stored == nil:
		spec.PolicyTiers = platformDefaultTiers
	case len(stored.GetPolicyTiers()) > 0:
		spec.PolicyTiers = stored.GetPolicyTiers()
		retained = 0
		for _, t := range stored.GetPolicyTiers() {
			if t.GetFrequency() == "weekly" && t.GetEnabled() {
				retained = max(t.GetRetentionValue(), 1)
			}
		}
		if retained == 0 {
			retained = 4
		}
	default:
		if n := stored.GetRetention().GetMaxBackups(); n > 0 {
			retained = n
		}
		spec.PolicyTiers = []*serverv1.PolicyTier{
			{Frequency: "daily", Interval: 1, RetentionValue: retained, RetentionUnit: "days", Enabled: true},
			{Frequency: "weekly", Interval: 1, RetentionValue: retained, RetentionUnit: "weeks", Enabled: true},
		}
	}
	if stored.GetSnapshotTime() != nil {
		spec.SnapshotTime = stored.GetSnapshotTime()
	}
	spec.Retention = &serverv1.RetentionPolicy{MaxBackups: retained}
	if db.GetSpec().GetEngine() == enginePostgreSQL {
		spec.PitrWindowDays = 7
		if stored.GetPitrWindowDays() > 0 {
			spec.PitrWindowDays = stored.GetPitrWindowDays()
		}
	}
	return &serverv1.BackupSchedule{
		Id:              "sched-" + db.GetId(),
		WorkspaceId:     db.GetWorkspaceId(),
		DatabaseId:      db.GetId(),
		Spec:            spec,
		Managed:         true,
		PlatformDefault: stored == nil,
		Status: &serverv1.BackupScheduleStatus{
			Phase:          "Active",
			LastBackupTime: timestamppb.New(time.Unix(1_700_000_000, 0)),
			NextBackupTime: timestamppb.New(time.Unix(1_700_086_400, 0)),
			ActiveBackups:  3,
		},
	}
}
