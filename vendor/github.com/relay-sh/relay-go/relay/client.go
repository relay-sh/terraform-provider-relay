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

type Options struct {
	Endpoint  string
	Token     string
	Insecure  bool
	Origin    string
	UserAgent string
}

type Client struct {
	conn   *grpc.ClientConn
	Server serverv1.ServerServiceClient
}

var (
	ErrMissingEndpoint = errors.New("relay: endpoint is required")
	ErrMissingToken    = errors.New("relay: token is required")
)

func Dial(opts Options) (*Client, error) {
	if opts.Endpoint == "" {
		return nil, ErrMissingEndpoint
	}
	if opts.Token == "" {
		return nil, ErrMissingToken
	}
	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(transportCredentials(opts.Endpoint, opts.Insecure)),
		grpc.WithPerRPCCredentials(tokenAuth{token: opts.Token, origin: opts.Origin, requireTLS: !opts.Insecure}),
	}
	if opts.UserAgent != "" {
		dialOpts = append(dialOpts, grpc.WithUserAgent(opts.UserAgent))
	}
	conn, err := grpc.NewClient(opts.Endpoint, dialOpts...)
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
