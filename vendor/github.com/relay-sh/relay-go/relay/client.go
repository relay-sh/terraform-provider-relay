package relay

import (
	"crypto/tls"
	"errors"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

const DefaultEndpoint = "api.userelay.sh:443"

type Options struct {
	Endpoint    string
	Credentials Credentials
	Insecure    bool
	Origin      string
	UserAgent   string
}

type Client struct {
	conn   *grpc.ClientConn
	Server serverv1.ServerServiceClient
}

var ErrMissingCredentials = errors.New("relay: credentials are required")

func Dial(opts Options) (*Client, error) {
	if opts.Credentials == nil {
		return nil, ErrMissingCredentials
	}
	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(transportCredentials(endpoint, opts.Insecure)),
		grpc.WithPerRPCCredentials(perRPC{creds: opts.Credentials, origin: opts.Origin, requireTLS: !opts.Insecure}),
	}
	if opts.UserAgent != "" {
		dialOpts = append(dialOpts, grpc.WithUserAgent(opts.UserAgent))
	}
	conn, err := grpc.NewClient(endpoint, dialOpts...)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, Server: serverv1.NewServerServiceClient(conn)}, nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

func transportCredentials(endpoint string, plaintext bool) credentials.TransportCredentials {
	if plaintext {
		return insecure.NewCredentials()
	}
	return credentials.NewTLS(&tls.Config{ServerName: hostOf(endpoint), MinVersion: tls.VersionTLS12})
}

func hostOf(endpoint string) string {
	if host, _, err := net.SplitHostPort(endpoint); err == nil {
		return host
	}
	return endpoint
}
