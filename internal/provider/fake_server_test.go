package provider

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

type call struct {
	method    string
	org       string
	workspace string
	origin    string
}

type fakeServer struct {
	serverv1.UnimplementedServerServiceServer

	mu    sync.Mutex
	addr  string
	calls []call
	errs  map[string]error

	orgs        map[string]*serverv1.Organization
	workspaces  map[string]*serverv1.Workspace
	connections map[string]*serverv1.Connection
	lastCreate  *serverv1.CreateConnectionRequest
	lastUpdate  *serverv1.UpdateConnectionRequest
	nextID      int
}

func startFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeServer{
		addr:        lis.Addr().String(),
		errs:        map[string]error{},
		orgs:        map[string]*serverv1.Organization{},
		workspaces:  map[string]*serverv1.Workspace{},
		connections: map[string]*serverv1.Connection{},
	}
	srv := grpc.NewServer()
	serverv1.RegisterServerServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return f
}

func (f *fakeServer) failWith(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[method] = err
}

func (f *fakeServer) record(ctx context.Context, method string) error {
	md, _ := metadata.FromIncomingContext(ctx)
	first := func(k string) string {
		if v := md.Get(k); len(v) > 0 {
			return v[0]
		}
		return ""
	}
	f.calls = append(f.calls, call{method: method, org: first("x-relay-org-id"), workspace: first("x-relay-workspace-id"), origin: first("x-relay-origin")})
	return f.errs[method]
}

func (f *fakeServer) callsTo(method string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeServer) GetOrganization(ctx context.Context, req *serverv1.GetOrganizationRequest) (*serverv1.GetOrganizationResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "GetOrganization"); err != nil {
		return nil, err
	}
	if req.GetKey() == nil {
		if o, ok := f.orgs[testOrgID]; ok {
			return &serverv1.GetOrganizationResponse{Organization: o}, nil
		}
	}
	for _, o := range f.orgs {
		if o.GetId() == req.GetId() || (req.GetSlug() != "" && o.GetSlug() == req.GetSlug()) {
			return &serverv1.GetOrganizationResponse{Organization: o}, nil
		}
	}
	return nil, status.Error(codes.NotFound, "organization not found")
}

func (f *fakeServer) newID(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s-%d", prefix, f.nextID)
}

func (f *fakeServer) workspaceByName(name string) *serverv1.Workspace {
	for _, w := range f.workspaces {
		if w.GetName() == name {
			return w
		}
	}
	return nil
}

func (f *fakeServer) CreateWorkspace(ctx context.Context, req *serverv1.CreateWorkspaceRequest) (*serverv1.CreateWorkspaceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "CreateWorkspace"); err != nil {
		return nil, err
	}
	if f.workspaceByName(req.GetName()) != nil {
		return nil, status.Errorf(codes.AlreadyExists, "workspace %q already exists", req.GetName())
	}
	w := &serverv1.Workspace{Id: f.newID("ws"), Name: req.GetName(), Slug: req.GetName(), Description: req.GetDescription(), ManagedBy: req.GetManagedBy()}
	f.workspaces[w.Id] = w
	return &serverv1.CreateWorkspaceResponse{Workspace: w}, nil
}

func (f *fakeServer) GetWorkspace(ctx context.Context, req *serverv1.GetWorkspaceRequest) (*serverv1.GetWorkspaceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "GetWorkspace"); err != nil {
		return nil, err
	}
	w := f.workspaces[req.GetId()]
	if req.GetName() != "" {
		w = f.workspaceByName(req.GetName())
	}
	if w == nil {
		return nil, status.Error(codes.NotFound, "workspace not found")
	}
	return &serverv1.GetWorkspaceResponse{Workspace: w}, nil
}

func (f *fakeServer) UpdateWorkspace(ctx context.Context, req *serverv1.UpdateWorkspaceRequest) (*serverv1.UpdateWorkspaceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "UpdateWorkspace"); err != nil {
		return nil, err
	}
	w := f.workspaceByName(req.GetName())
	if w == nil {
		return nil, status.Error(codes.NotFound, "workspace not found")
	}
	if req.NewName != nil {
		w.Name, w.Slug = req.GetNewName(), req.GetNewName()
	}
	if req.Description != nil {
		w.Description = req.GetDescription()
	}
	if req.ManagedBy != nil {
		w.ManagedBy = req.GetManagedBy()
	}
	return &serverv1.UpdateWorkspaceResponse{Workspace: w}, nil
}

func (f *fakeServer) DeleteWorkspace(ctx context.Context, req *serverv1.DeleteWorkspaceRequest) (*serverv1.DeleteWorkspaceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "DeleteWorkspace"); err != nil {
		return nil, err
	}
	w := f.workspaceByName(req.GetName())
	if w == nil {
		return nil, status.Error(codes.NotFound, "workspace not found")
	}
	delete(f.workspaces, w.Id)
	return &serverv1.DeleteWorkspaceResponse{}, nil
}

func workspaceOf(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get("x-relay-workspace-id"); len(v) > 0 {
		return v[0]
	}
	return ""
}

func connectionKey(ctx context.Context, name string) string {
	return workspaceOf(ctx) + "/" + name
}

func normalizeTLS(c *serverv1.Connection) {
	if t := c.GetTls(); t != nil && t.GetCert() == "" && t.GetKey() == "" && t.GetCa() == "" {
		c.Tls = nil
	}
	if c.GetCredentialRef() != "" {
		c.PasswordSet = false
	}
}

func (f *fakeServer) CreateConnection(ctx context.Context, req *serverv1.CreateConnectionRequest) (*serverv1.CreateConnectionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "CreateConnection"); err != nil {
		return nil, err
	}
	f.lastCreate = req
	key := connectionKey(ctx, req.GetConnection().GetName())
	if _, ok := f.connections[key]; ok {
		return nil, status.Error(codes.AlreadyExists, "connection already exists")
	}
	c := proto.Clone(req.GetConnection()).(*serverv1.Connection)
	c.Id = f.newID("conn")
	c.Status = "unknown"
	c.PasswordSet = c.GetPassword() != ""
	c.Password = ""
	if c.GetKind() == "" {
		c.Kind = "interactive"
	}
	normalizeTLS(c)
	f.connections[key] = c
	return &serverv1.CreateConnectionResponse{Connection: c}, nil
}

func (f *fakeServer) GetConnections(ctx context.Context, req *serverv1.GetConnectionsRequest) (*serverv1.GetConnectionsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "GetConnections"); err != nil {
		return nil, err
	}
	out := &serverv1.GetConnectionsResponse{}
	for _, name := range req.GetNames() {
		c, ok := f.connections[connectionKey(ctx, name)]
		if !ok {
			return nil, status.Errorf(codes.NotFound, "connection %q not found", name)
		}
		out.Connections = append(out.Connections, c)
	}
	return out, nil
}

func (f *fakeServer) UpdateConnection(ctx context.Context, req *serverv1.UpdateConnectionRequest) (*serverv1.UpdateConnectionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "UpdateConnection"); err != nil {
		return nil, err
	}
	f.lastUpdate = req
	c, ok := f.connections[connectionKey(ctx, req.GetName())]
	if !ok {
		return nil, status.Error(codes.NotFound, "connection not found")
	}
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&c.Engine, req.Engine)
	set(&c.Host, req.Host)
	set(&c.User, req.User)
	set(&c.CredentialRef, req.CredentialRef)
	set(&c.CredentialSource, req.CredentialSource)
	set(&c.Database, req.Database)
	set(&c.Listen, req.Listen)
	set(&c.AgentPool, req.AgentPool)
	set(&c.Kind, req.Kind)
	set(&c.ManagedBy, req.ManagedBy)
	if req.Password != nil {
		c.PasswordSet = req.GetPassword() != ""
	}
	if c.Config == nil {
		c.Config = &serverv1.ConnectionConfig{}
	}
	if req.RedactEnabled != nil {
		c.Config.RedactEnabled = req.GetRedactEnabled()
	}
	if req.AuditEnabled != nil {
		c.Config.AuditEnabled = req.GetAuditEnabled()
	}
	set(&c.Config.PolicyFailMode, req.PolicyFailMode)
	if req.GetClearRedactRules() {
		c.Config.RedactRules = nil
	} else if len(req.GetRedactRules()) > 0 {
		c.Config.RedactRules = req.GetRedactRules()
	}
	if req.GetClearManagedDatabaseId() {
		c.ManagedDatabaseId = ""
	} else {
		set(&c.ManagedDatabaseId, req.ManagedDatabaseId)
	}
	if req.Tls != nil {
		c.Tls = req.GetTls()
	}
	if req.HostTls != nil {
		c.HostTls = req.GetHostTls()
	}
	normalizeTLS(c)
	return &serverv1.UpdateConnectionResponse{Connection: c}, nil
}

func (f *fakeServer) DeleteConnection(ctx context.Context, req *serverv1.DeleteConnectionRequest) (*serverv1.DeleteConnectionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "DeleteConnection"); err != nil {
		return nil, err
	}
	key := connectionKey(ctx, req.GetName())
	if _, ok := f.connections[key]; !ok {
		return nil, status.Error(codes.NotFound, "connection not found")
	}
	delete(f.connections, key)
	return &serverv1.DeleteConnectionResponse{}, nil
}
