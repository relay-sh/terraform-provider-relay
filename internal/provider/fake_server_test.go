package provider

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
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

	databases         map[string]*serverv1.Database
	dbCountdown       map[string]int
	readyAfter        int
	stuck             bool
	failProvision     string
	failDeletion      string
	failWhileDeleting error
	lastDBCreate      *serverv1.CreateDatabaseRequest
	lastDBUpdate      *serverv1.UpdateDatabaseRequest
	lastDBDelete      *serverv1.DeleteDatabaseRequest
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
		databases:   map[string]*serverv1.Database{},
		dbCountdown: map[string]int{},
		readyAfter:  2,
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

func (f *fakeServer) settle(id, phase string) {
	f.databases[id].Status.Phase = phase
	f.dbCountdown[id] = f.readyAfter
}

func (f *fakeServer) CreateDatabase(ctx context.Context, req *serverv1.CreateDatabaseRequest) (*serverv1.CreateDatabaseResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "CreateDatabase"); err != nil {
		return nil, err
	}
	f.lastDBCreate = req
	for _, db := range f.databases {
		if db.GetWorkspaceId() == workspaceOf(ctx) && db.GetName() == req.GetName() {
			return nil, status.Error(codes.AlreadyExists, "database already exists")
		}
	}
	spec := proto.Clone(req.GetSpec()).(*serverv1.DatabaseSpec)
	for _, n := range spec.GetNodes() {
		if n.GetResources().GetStorageClass() == "" {
			n.Resources.StorageClass = "standard"
		}
	}
	if spec.Replication == nil {
		spec.Replication = &serverv1.Replication{Mode: "async"}
	}
	db := &serverv1.Database{
		Id:          f.newID("db"),
		Name:        req.GetName(),
		OrgId:       testOrgID,
		WorkspaceId: workspaceOf(ctx),
		Spec:        spec,
		Status: &serverv1.DatabaseStatus{
			Endpoint:        &serverv1.Endpoint{Host: req.GetName() + ".db.relay", Port: 5432},
			PublicEndpoints: []*serverv1.PublicEndpoint{{Role: "primary", Host: req.GetName() + ".public", Port: 5432}},
			TotalNodes:      1,
		},
	}
	f.databases[db.Id] = db
	f.settle(db.Id, "provisioning")
	return &serverv1.CreateDatabaseResponse{Database: db}, nil
}

func (f *fakeServer) GetDatabase(ctx context.Context, req *serverv1.GetDatabaseRequest) (*serverv1.GetDatabaseResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "GetDatabase"); err != nil {
		return nil, err
	}
	db, ok := f.databases[req.GetId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "database not found")
	}
	if db.Status.Phase == "deleting" && f.failWhileDeleting != nil {
		return nil, f.failWhileDeleting
	}
	if f.dbCountdown[db.Id] > 0 && !f.stuck {
		f.dbCountdown[db.Id]--
		if f.dbCountdown[db.Id] == 0 {
			switch db.Status.Phase {
			case "provisioning":
				db.Status.Phase, db.Status.ReadyNodes = "running", 1
				if f.failProvision != "" {
					db.Status.Phase, db.Status.Message = "failed", f.failProvision
				}
			case "deleting":
				if f.failDeletion != "" {
					db.Status.Phase, db.Status.Message = "failed", f.failDeletion
				} else {
					delete(f.databases, db.Id)
					return nil, status.Error(codes.NotFound, "database not found")
				}
			}
		}
	}
	return &serverv1.GetDatabaseResponse{Database: db}, nil
}

func (f *fakeServer) UpdateDatabase(ctx context.Context, req *serverv1.UpdateDatabaseRequest) (*serverv1.UpdateDatabaseResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "UpdateDatabase"); err != nil {
		return nil, err
	}
	f.lastDBUpdate = req
	db, ok := f.databases[req.GetId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "database not found")
	}
	in := req.GetSpec()
	for _, p := range req.GetUpdateMask().GetPaths() {
		switch p {
		case "topology":
			db.Spec.Topology = in.GetTopology()
		case "nodes":
			db.Spec.Nodes = in.GetNodes()
		case "params":
			db.Spec.Params = in.GetParams()
		case "backups_enabled":
			db.Spec.BackupsEnabled = in.GetBackupsEnabled()
		case "replication":
			db.Spec.Replication = in.GetReplication()
		case "postgresql":
			db.Spec.Postgresql = in.GetPostgresql()
		default:
			return nil, status.Errorf(codes.InvalidArgument, "path %q is not updatable", p)
		}
	}
	f.settle(db.Id, "provisioning")
	return &serverv1.UpdateDatabaseResponse{Database: db}, nil
}

func (f *fakeServer) DeleteDatabase(ctx context.Context, req *serverv1.DeleteDatabaseRequest) (*serverv1.DeleteDatabaseResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "DeleteDatabase"); err != nil {
		return nil, err
	}
	f.lastDBDelete = req
	if _, ok := f.databases[req.GetId()]; !ok {
		return nil, status.Error(codes.NotFound, "database not found")
	}
	f.settle(req.GetId(), "deleting")
	return &serverv1.DeleteDatabaseResponse{}, nil
}

func (f *fakeServer) ListDatabases(ctx context.Context, req *serverv1.ListDatabasesRequest) (*serverv1.ListDatabasesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "ListDatabases"); err != nil {
		return nil, err
	}
	var ids []string
	for id, db := range f.databases {
		if db.GetWorkspaceId() == workspaceOf(ctx) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	start := 0
	if req.GetPageToken() != "" {
		start, _ = strconv.Atoi(req.GetPageToken())
	}
	out := &serverv1.ListDatabasesResponse{}
	if start < len(ids) {
		out.Databases = []*serverv1.Database{f.databases[ids[start]]}
		if start+1 < len(ids) {
			out.NextPageToken = strconv.Itoa(start + 1)
		}
	}
	return out, nil
}
