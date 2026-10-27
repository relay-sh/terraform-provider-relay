package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

var serverRoleARN = regexp.MustCompile(`^arn:aws[a-z-]*:iam::(\d{12}):role/[\w+=,.@/-]+$`)

const fakePermissionsPolicy = `{"Statement":[{"Effect":"Allow","Action":["s3:PutObject"],"Resource":"arn:aws:s3:::YOUR_BUCKET/*"}]}`

func orgOf(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get("x-relay-org-id"); len(v) > 0 {
		return v[0]
	}
	return ""
}

func parseRoleARN(arn string) (string, error) {
	m := serverRoleARN.FindStringSubmatch(strings.TrimSpace(arn))
	if m == nil {
		return "", errors.New("role_arn must look like arn:aws:iam::123456789012:role/name")
	}
	return m[1], nil
}

func (f *fakeServer) findCloudIntegration(id string) (*serverv1.CloudIntegration, error) {
	if !uuidPattern.MatchString(id) {
		return nil, status.Error(codes.InvalidArgument, "id must be a UUID")
	}
	for _, it := range f.cloudIntegrations {
		if it.GetId() == id {
			return it, nil
		}
	}
	return nil, status.Error(codes.NotFound, "cloud integration not found")
}

func (f *fakeServer) cloudIntegrationNamed(name, except string) bool {
	return slices.ContainsFunc(f.cloudIntegrations, func(c *serverv1.CloudIntegration) bool { return c.GetName() == name && c.GetId() != except })
}

func (f *fakeServer) GetCloudIntegrationSetup(ctx context.Context, req *serverv1.GetCloudIntegrationSetupRequest) (*serverv1.GetCloudIntegrationSetupResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "GetCloudIntegrationSetup"); err != nil {
		return nil, err
	}
	if req.GetProvider() != "aws" {
		return nil, status.Errorf(codes.InvalidArgument, "provider %q is not available yet", req.GetProvider())
	}
	if f.relayAWSAccountID == "" {
		return nil, status.Error(codes.FailedPrecondition, "AWS integrations are not configured on this server")
	}
	externalID := "relay-" + orgOf(ctx)
	return &serverv1.GetCloudIntegrationSetupResponse{
		RelayAwsAccountId: f.relayAWSAccountID,
		ExternalId:        externalID,
		TrustPolicy:       fmt.Sprintf(`{"Principal":{"AWS":"arn:aws:iam::%s:root"},"Condition":{"StringEquals":{"sts:ExternalId":%q}}}`, f.relayAWSAccountID, externalID),
		PermissionsPolicy: fakePermissionsPolicy,
	}, nil
}

func (f *fakeServer) ListCloudIntegrations(ctx context.Context, _ *serverv1.ListCloudIntegrationsRequest) (*serverv1.ListCloudIntegrationsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "ListCloudIntegrations"); err != nil {
		return nil, err
	}
	return &serverv1.ListCloudIntegrationsResponse{Integrations: f.cloudIntegrations}, nil
}

func (f *fakeServer) GetCloudIntegration(ctx context.Context, req *serverv1.GetCloudIntegrationRequest) (*serverv1.GetCloudIntegrationResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "GetCloudIntegration"); err != nil {
		return nil, err
	}
	it, err := f.findCloudIntegration(req.GetId())
	if err != nil {
		return nil, err
	}
	return &serverv1.GetCloudIntegrationResponse{Integration: proto.Clone(it).(*serverv1.CloudIntegration)}, nil
}

func (f *fakeServer) CreateCloudIntegration(ctx context.Context, req *serverv1.CreateCloudIntegrationRequest) (*serverv1.CreateCloudIntegrationResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "CreateCloudIntegration"); err != nil {
		return nil, err
	}
	if req.GetProvider() != "aws" {
		return nil, status.Errorf(codes.InvalidArgument, "provider %q is not available yet", req.GetProvider())
	}
	name := strings.TrimSpace(req.GetName())
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	account, err := parseRoleARN(req.GetRoleArn())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if f.cloudIntegrationNamed(name, "") {
		return nil, status.Errorf(codes.AlreadyExists, "cloud integration %q already exists", name)
	}
	f.nextID++
	it := &serverv1.CloudIntegration{
		Id:           fmt.Sprintf("00000000-0000-4000-8000-%012d", f.nextID),
		Provider:     "aws",
		Name:         name,
		AwsAccountId: account,
		RoleArn:      strings.TrimSpace(req.GetRoleArn()),
		ExternalId:   "relay-" + orgOf(ctx),
		Status:       "pending",
	}
	f.cloudIntegrations = append(f.cloudIntegrations, it)
	return &serverv1.CreateCloudIntegrationResponse{Integration: proto.Clone(it).(*serverv1.CloudIntegration)}, nil
}

func (f *fakeServer) UpdateCloudIntegration(ctx context.Context, req *serverv1.UpdateCloudIntegrationRequest) (*serverv1.UpdateCloudIntegrationResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "UpdateCloudIntegration"); err != nil {
		return nil, err
	}
	if !uuidPattern.MatchString(req.GetId()) {
		return nil, status.Error(codes.InvalidArgument, "id must be a UUID")
	}
	updated := &serverv1.CloudIntegration{}
	if req.Name != nil {
		updated.Name = strings.TrimSpace(req.GetName())
		if updated.Name == "" {
			return nil, status.Error(codes.InvalidArgument, "name must not be empty")
		}
	}
	if req.RoleArn != nil {
		account, err := parseRoleARN(req.GetRoleArn())
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		updated.RoleArn, updated.AwsAccountId = strings.TrimSpace(req.GetRoleArn()), account
	}
	if req.Name == nil && req.RoleArn == nil {
		return nil, status.Error(codes.InvalidArgument, "nothing to update")
	}
	it, err := f.findCloudIntegration(req.GetId())
	if err != nil {
		return nil, err
	}
	if req.Name != nil && f.cloudIntegrationNamed(updated.Name, it.GetId()) {
		return nil, status.Errorf(codes.AlreadyExists, "cloud integration %q already exists", req.GetName())
	}
	if req.Name != nil {
		it.Name = updated.Name
	}
	if req.RoleArn != nil {
		it.RoleArn, it.AwsAccountId, it.Status, it.LastError = updated.RoleArn, updated.AwsAccountId, "pending", ""
	}
	return &serverv1.UpdateCloudIntegrationResponse{Integration: proto.Clone(it).(*serverv1.CloudIntegration)}, nil
}

func (f *fakeServer) DeleteCloudIntegration(ctx context.Context, req *serverv1.DeleteCloudIntegrationRequest) (*serverv1.DeleteCloudIntegrationResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "DeleteCloudIntegration"); err != nil {
		return nil, err
	}
	it, err := f.findCloudIntegration(req.GetId())
	if err != nil {
		return nil, err
	}
	if f.autoExport.GetIntegrationId() == it.GetId() {
		return nil, status.Errorf(codes.FailedPrecondition, "cloud integration %q is still used by one or more exports", it.GetName())
	}
	f.cloudIntegrations = slices.DeleteFunc(f.cloudIntegrations, func(c *serverv1.CloudIntegration) bool { return c.GetId() == it.GetId() })
	return &serverv1.DeleteCloudIntegrationResponse{}, nil
}

func (f *fakeServer) TestCloudIntegration(ctx context.Context, req *serverv1.TestCloudIntegrationRequest) (*serverv1.TestCloudIntegrationResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "TestCloudIntegration"); err != nil {
		return nil, err
	}
	if !uuidPattern.MatchString(req.GetId()) {
		return nil, status.Error(codes.InvalidArgument, "id must be a UUID")
	}
	if f.relayAWSAccountID == "" {
		return nil, status.Error(codes.FailedPrecondition, "AWS integrations are not configured on this server")
	}
	it, err := f.findCloudIntegration(req.GetId())
	if err != nil {
		return nil, err
	}
	it.Status, it.LastError = "connected", ""
	if f.assumeRoleError != "" {
		it.Status, it.LastError = "error", f.assumeRoleError
	}
	return &serverv1.TestCloudIntegrationResponse{Integration: proto.Clone(it).(*serverv1.CloudIntegration), Ok: f.assumeRoleError == ""}, nil
}
