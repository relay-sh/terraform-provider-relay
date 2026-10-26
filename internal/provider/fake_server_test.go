package provider

import (
	"context"
	"fmt"
	"net"
	"slices"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

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
	skip  map[string]int

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
	users             map[string]*serverv1.DatabaseUser
	userCountdown     map[string]int
	failUser          string
	userWithoutTimes  bool
	lastUserCreate    *serverv1.CreateDatabaseUserRequest
	lastUserUpdate    *serverv1.UpdateDatabaseUserRequest
	logicalDBs        map[string]*serverv1.LogicalDatabase
	ldbCountdown      map[string]int
	failLogicalDB     string
	unlistedFor       int
	lastLDBCreate     *serverv1.CreateLogicalDatabaseRequest
	lastLDBUpdate     *serverv1.UpdateLogicalDatabaseRequest
	providers         []*serverv1.DatabaseProvider
	lastDBCreate      *serverv1.CreateDatabaseRequest
	lastDBUpdate      *serverv1.UpdateDatabaseRequest
	lastDBDelete      *serverv1.DeleteDatabaseRequest
	apiKeys           map[string]*serverv1.APIKey
	callerKey         *serverv1.APIKey
	lastKeyCreate     *serverv1.CreateAPIKeyRequest
	lastKeyUpdate     *serverv1.UpdateAPIKeyRequest

	allowlist       map[string][]*serverv1.IPAllowlistEntry
	syncingFor      int
	syncCountdown   map[string]int
	lastAllowUpdate *serverv1.UpdateIPAllowlistEntryRequest

	peServices     map[string]*serverv1.PrivateEndpointService
	peRegions      []*serverv1.PrivateEndpointRegion
	initiatingFor  int
	failService    string
	serviceBlocked int
	svcCountdown   map[string]int
	pendingFor     int
	deletingFor    int
	rejectEndpoint string
	epCountdown    map[string]int
}

func startFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeServer{
		addr:          lis.Addr().String(),
		errs:          map[string]error{},
		skip:          map[string]int{},
		orgs:          map[string]*serverv1.Organization{},
		workspaces:    map[string]*serverv1.Workspace{},
		connections:   map[string]*serverv1.Connection{},
		databases:     map[string]*serverv1.Database{},
		dbCountdown:   map[string]int{},
		readyAfter:    2,
		users:         map[string]*serverv1.DatabaseUser{},
		userCountdown: map[string]int{},
		logicalDBs:    map[string]*serverv1.LogicalDatabase{},
		ldbCountdown:  map[string]int{},
		apiKeys:       map[string]*serverv1.APIKey{},
		allowlist:     map[string][]*serverv1.IPAllowlistEntry{},
		syncCountdown: map[string]int{},
		peServices:    map[string]*serverv1.PrivateEndpointService{},
		svcCountdown:  map[string]int{},
		epCountdown:   map[string]int{},
		peRegions:     []*serverv1.PrivateEndpointRegion{{Provider: "aws", Region: "us-east-1"}},
		providers: []*serverv1.DatabaseProvider{
			{Cloud: "aws", Region: "us-east-1", Engines: []string{"PostgreSQL"}},
			{Cloud: "aws", Region: "eu-west-1", Engines: []string{"PostgreSQL"}},
			{Cloud: "aws", Region: "eu-west-1", Engines: []string{"MongoDB"}},
		},
	}
	srv := grpc.NewServer(grpc.UnaryInterceptor(f.snapshot))
	serverv1.RegisterServerServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return f
}

func (f *fakeServer) snapshot(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	resp, err := handler(ctx, req)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return proto.Clone(resp.(proto.Message)), nil
}

func (f *fakeServer) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *fakeServer) failAfter(method string, calls int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[method], f.skip[method] = err, calls
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
	if f.skip[method] > 0 {
		f.skip[method]--
		return nil
	}
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
	normalizeTLS(c)
	f.connections[key] = c
	return &serverv1.CreateConnectionResponse{Connection: userFacing(c)}, nil
}

func userFacing(c *serverv1.Connection) *serverv1.Connection {
	out := proto.Clone(c).(*serverv1.Connection)
	if out.GetManagedDatabaseId() != "" {
		out.Host = ""
	}
	return out
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
		out.Connections = append(out.Connections, userFacing(c))
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
	return &serverv1.UpdateConnectionResponse{Connection: userFacing(c)}, nil
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

func validateNodes(spec *serverv1.DatabaseSpec) error {
	for _, n := range spec.GetNodes() {
		if n.GetRole() != roleElectable && n.GetRole() != roleStandby && n.GetRole() != roleReadOnly {
			return status.Errorf(codes.InvalidArgument, "validate spec: invalid role %q", n.GetRole())
		}
	}
	nodes := spec.GetNodes()
	if len(nodes) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var primary *serverv1.NodeSpec
	for i, n := range nodes {
		if n.GetName() == "" {
			return status.Errorf(codes.InvalidArgument, "validate spec: nodes[%d]: name is required", i)
		}
		if seen[n.GetName()] {
			return status.Errorf(codes.InvalidArgument, "validate spec: nodes[%d]: duplicate name %q", i, n.GetName())
		}
		seen[n.GetName()] = true
		if n.GetName() == spec.GetPrimaryNodeGroup() {
			primary = n
		}
	}
	if primary == nil {
		return status.Errorf(codes.InvalidArgument, "validate spec: primaryNodeGroup %q does not name any node group", spec.GetPrimaryNodeGroup())
	}
	if primary.GetRole() != roleElectable {
		return status.Errorf(codes.InvalidArgument, "validate spec: primaryNodeGroup %q must have role %q, got %q", primary.GetName(), roleElectable, primary.GetRole())
	}
	return nil
}

func validateValkey(spec *serverv1.DatabaseSpec) error {
	if spec.GetEngine() != engineValkey {
		return nil
	}
	if spec.GetReplication() != nil {
		return status.Error(codes.InvalidArgument, "validate spec: replication tuning is not supported for Valkey")
	}
	if len(spec.GetNodes()) != 1 {
		return status.Errorf(codes.InvalidArgument, "validate spec: exactly one node group is supported for Valkey, got %d", len(spec.GetNodes()))
	}
	v, count := spec.GetValkey(), spec.GetNodes()[0].GetCount()
	switch v.GetMode() {
	case valkeyModeReplication:
		if v.GetShards() != 0 || v.GetReplicasPerShard() != 0 {
			return status.Error(codes.InvalidArgument, "validate spec: shards and replicasPerShard apply only to mode cluster")
		}
	case valkeyModeCluster:
		if want := v.GetShards() * (1 + v.GetReplicasPerShard()); count != want {
			return status.Errorf(codes.InvalidArgument, "validate spec: node count must be shards * (1 + replicasPerShard) = %d, got %d", want, count)
		}
	default:
		return status.Errorf(codes.InvalidArgument, "validate spec: mode must be replication or cluster, got %q", v.GetMode())
	}
	return nil
}

// bindPlacement mirrors the platform: a group is placed by cloud and region, and
// a pair no provisioner serves is rejected.
func (f *fakeServer) bindPlacement(nodes []*serverv1.NodeSpec) error {
	for i, n := range nodes {
		if n.GetCloud() == "" {
			return status.Errorf(codes.InvalidArgument, "nodes[%d].cloud is required", i)
		}
		if n.GetRegion() == "" {
			return status.Errorf(codes.InvalidArgument, "nodes[%d].region is required", i)
		}
		served := slices.ContainsFunc(f.providers, func(p *serverv1.DatabaseProvider) bool {
			return p.GetCloud() == n.GetCloud() && p.GetRegion() == n.GetRegion()
		})
		if !served {
			return status.Errorf(codes.InvalidArgument, "nodes[%d]: no provider registered for %s/%s", i, n.GetCloud(), n.GetRegion())
		}
	}
	return nil
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
	if err := validateNodes(req.GetSpec()); err != nil {
		return nil, err
	}
	if err := validateValkey(req.GetSpec()); err != nil {
		return nil, err
	}
	for _, db := range f.databases {
		if db.GetWorkspaceId() == workspaceOf(ctx) && db.GetName() == req.GetName() {
			return nil, status.Error(codes.AlreadyExists, "database already exists")
		}
	}
	spec := proto.Clone(req.GetSpec()).(*serverv1.DatabaseSpec)
	if err := f.bindPlacement(spec.GetNodes()); err != nil {
		return nil, err
	}
	for _, n := range spec.GetNodes() {
		if n.GetResources().GetStorageClass() == "" {
			n.Resources.StorageClass = "standard"
		}
	}
	port := int32(5432)
	if spec.GetEngine() == engineValkey {
		port = 6379
	} else if spec.Replication == nil {
		spec.Replication = &serverv1.Replication{Mode: "async"}
	}
	db := &serverv1.Database{
		Id:          f.newID("db"),
		Name:        req.GetName(),
		OrgId:       testOrgID,
		WorkspaceId: workspaceOf(ctx),
		Spec:        spec,
		Status: &serverv1.DatabaseStatus{
			Endpoint:        &serverv1.Endpoint{Host: req.GetName() + ".public", Port: port},
			PublicEndpoints: []*serverv1.PublicEndpoint{{Role: "rw", Host: req.GetName() + ".public", Port: port}},
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
	merged := proto.Clone(db.GetSpec()).(*serverv1.DatabaseSpec)
	for _, p := range req.GetUpdateMask().GetPaths() {
		switch p {
		case "nodes":
			if err := validateNodes(in); err != nil {
				return nil, err
			}
			if err := f.bindPlacement(in.GetNodes()); err != nil {
				return nil, err
			}
			merged.Nodes = in.GetNodes()
		case "replication":
			merged.Replication = in.GetReplication()
		case "postgresql":
			merged.Postgresql = in.GetPostgresql()
		case "valkey":
			merged.Valkey = in.GetValkey()
		default:
			return nil, status.Errorf(codes.InvalidArgument, "path %q is not updatable", p)
		}
	}
	if err := validateValkey(merged); err != nil {
		return nil, err
	}
	db.Spec = merged
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
	db, ok := f.databases[req.GetId()]
	// The server resolves the cluster in the caller's workspace and answers NotFound
	// outside it, so a delete without the workspace scope must not find anything.
	if !ok || db.GetWorkspaceId() != workspaceOf(ctx) {
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

func (f *fakeServer) ListDatabaseProviders(ctx context.Context, _ *serverv1.ListDatabaseProvidersRequest) (*serverv1.ListDatabaseProvidersResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "ListDatabaseProviders"); err != nil {
		return nil, err
	}
	return &serverv1.ListDatabaseProvidersResponse{Providers: f.providers}, nil
}

func userKey(databaseID, username string) string {
	return databaseID + "/" + username
}

func (f *fakeServer) CreateDatabaseUser(ctx context.Context, req *serverv1.CreateDatabaseUserRequest) (*serverv1.CreateDatabaseUserResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "CreateDatabaseUser"); err != nil {
		return nil, err
	}
	f.lastUserCreate = req
	key := userKey(req.GetDatabaseId(), req.GetUsername())
	if _, ok := f.users[key]; ok {
		return nil, status.Error(codes.AlreadyExists, "user already exists")
	}
	f.users[key] = &serverv1.DatabaseUser{
		Username:            req.GetUsername(),
		Description:         req.GetDescription(),
		AuthMethod:          "password",
		Roles:               req.GetRoles(),
		RestrictedResources: req.GetRestrictedResources(),
		ExpiresAt:           req.GetExpiresAt(),
		ConnectionLimit:     req.ConnectionLimit,
		StatementTimeout:    req.GetStatementTimeout(),
		Phase:               "Pending",
		CreatedAt:           timestamppb.New(time.Unix(1_700_000_000, 0)),
	}
	if f.userWithoutTimes {
		f.users[key].CreatedAt = nil
	}
	f.userCountdown[key] = f.readyAfter
	out := &serverv1.CreateDatabaseUserResponse{User: f.users[key]}
	if req.GetGeneratePassword() {
		out.GeneratedPassword = "generated-secret"
	}
	return out, nil
}

func (f *fakeServer) GetDatabaseUser(ctx context.Context, req *serverv1.GetDatabaseUserRequest) (*serverv1.GetDatabaseUserResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "GetDatabaseUser"); err != nil {
		return nil, err
	}
	key := userKey(req.GetDatabaseId(), req.GetUsername())
	u, ok := f.users[key]
	if !ok {
		return nil, status.Error(codes.NotFound, "user not found")
	}
	if f.userCountdown[key] > 0 && !f.stuck {
		f.userCountdown[key]--
		if f.userCountdown[key] == 0 {
			u.Phase = "Ready"
			if f.failUser != "" {
				u.Phase, u.PhaseMessage = "Failed", f.failUser
			}
		}
	}
	return &serverv1.GetDatabaseUserResponse{User: u}, nil
}

func (f *fakeServer) UpdateDatabaseUser(ctx context.Context, req *serverv1.UpdateDatabaseUserRequest) (*serverv1.UpdateDatabaseUserResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "UpdateDatabaseUser"); err != nil {
		return nil, err
	}
	f.lastUserUpdate = req
	u, ok := f.users[userKey(req.GetDatabaseId(), req.GetUsername())]
	if !ok {
		return nil, status.Error(codes.NotFound, "user not found")
	}
	if len(req.GetRoles()) > 0 {
		u.Roles = req.GetRoles()
	}
	if req.GetDescription() != "" {
		u.Description = req.GetDescription()
	}
	if req.GetStatementTimeout() != "" {
		u.StatementTimeout = req.GetStatementTimeout()
	}
	if req.ConnectionLimit != nil {
		u.ConnectionLimit = req.ConnectionLimit
	}
	if t := req.GetExpiresAt(); t != nil {
		u.ExpiresAt = t
		if t.AsTime().IsZero() {
			u.ExpiresAt = nil
		}
	}
	updated := proto.Clone(u).(*serverv1.DatabaseUser)
	updated.CreatedAt = nil
	return &serverv1.UpdateDatabaseUserResponse{User: updated}, nil
}

func (f *fakeServer) DeleteDatabaseUser(ctx context.Context, req *serverv1.DeleteDatabaseUserRequest) (*serverv1.DeleteDatabaseUserResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "DeleteDatabaseUser"); err != nil {
		return nil, err
	}
	if req.GetUsername() == "" {
		return nil, status.Error(codes.InvalidArgument, "username is required")
	}
	key := userKey(req.GetDatabaseId(), req.GetUsername())
	if _, ok := f.users[key]; !ok {
		return nil, status.Error(codes.NotFound, "user not found")
	}
	delete(f.users, key)
	return &serverv1.DeleteDatabaseUserResponse{}, nil
}

func (f *fakeServer) CreateLogicalDatabase(ctx context.Context, req *serverv1.CreateLogicalDatabaseRequest) (*serverv1.CreateLogicalDatabaseResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "CreateLogicalDatabase"); err != nil {
		return nil, err
	}
	f.lastLDBCreate = req
	if req.GetName() == "postgres" {
		return nil, status.Error(codes.InvalidArgument, `database name "postgres" is reserved`)
	}
	key := userKey(req.GetDatabaseId(), req.GetName())
	if _, ok := f.logicalDBs[key]; ok {
		return nil, status.Error(codes.AlreadyExists, "database already exists")
	}
	f.logicalDBs[key] = &serverv1.LogicalDatabase{
		Name:        req.GetName(),
		Owner:       req.GetOwner(),
		Description: req.GetDescription(),
		Phase:       "Pending",
		Origin:      "Created",
		CreatedAt:   timestamppb.New(time.Unix(1_700_000_000, 0)),
	}
	f.ldbCountdown[key] = f.readyAfter
	return &serverv1.CreateLogicalDatabaseResponse{Database: f.logicalDBs[key]}, nil
}

func (f *fakeServer) ListLogicalDatabases(ctx context.Context, req *serverv1.ListLogicalDatabasesRequest) (*serverv1.ListLogicalDatabasesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "ListLogicalDatabases"); err != nil {
		return nil, err
	}
	if f.unlistedFor > 0 {
		f.unlistedFor--
		return &serverv1.ListLogicalDatabasesResponse{}, nil
	}
	out := &serverv1.ListLogicalDatabasesResponse{}
	for key, db := range f.logicalDBs {
		if key != userKey(req.GetDatabaseId(), db.GetName()) {
			continue
		}
		if f.ldbCountdown[key] > 0 && !f.stuck {
			f.ldbCountdown[key]--
			if f.ldbCountdown[key] == 0 {
				db.Phase = "Ready"
				if f.failLogicalDB != "" {
					db.Phase, db.PhaseMessage = "Failed", f.failLogicalDB
				}
			}
		}
		out.Databases = append(out.Databases, db)
	}
	sort.Slice(out.Databases, func(i, j int) bool { return out.Databases[i].GetName() < out.Databases[j].GetName() })
	return out, nil
}

func (f *fakeServer) UpdateLogicalDatabase(ctx context.Context, req *serverv1.UpdateLogicalDatabaseRequest) (*serverv1.UpdateLogicalDatabaseResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "UpdateLogicalDatabase"); err != nil {
		return nil, err
	}
	f.lastLDBUpdate = req
	db, ok := f.logicalDBs[userKey(req.GetDatabaseId(), req.GetName())]
	if !ok {
		return nil, status.Error(codes.NotFound, "database not found")
	}
	if req.Owner != nil {
		db.Owner = req.GetOwner()
	}
	if req.Description != nil {
		db.Description = req.GetDescription()
	}
	updated := proto.Clone(db).(*serverv1.LogicalDatabase)
	updated.CreatedAt = nil
	return &serverv1.UpdateLogicalDatabaseResponse{Database: updated}, nil
}

func (f *fakeServer) DeleteLogicalDatabase(ctx context.Context, req *serverv1.DeleteLogicalDatabaseRequest) (*serverv1.DeleteLogicalDatabaseResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "DeleteLogicalDatabase"); err != nil {
		return nil, err
	}
	key := userKey(req.GetDatabaseId(), req.GetName())
	if _, ok := f.logicalDBs[key]; !ok {
		return nil, status.Error(codes.NotFound, "database not found")
	}
	delete(f.logicalDBs, key)
	return &serverv1.DeleteLogicalDatabaseResponse{}, nil
}
