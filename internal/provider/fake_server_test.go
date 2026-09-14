package provider

import (
	"context"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

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

	orgs map[string]*serverv1.Organization
}

func startFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeServer{
		addr: lis.Addr().String(),
		errs: map[string]error{},
		orgs: map[string]*serverv1.Organization{},
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
	for _, o := range f.orgs {
		if o.GetId() == req.GetId() || (req.GetSlug() != "" && o.GetSlug() == req.GetSlug()) {
			return &serverv1.GetOrganizationResponse{Organization: o}, nil
		}
	}
	return nil, status.Error(codes.NotFound, "organization not found")
}
