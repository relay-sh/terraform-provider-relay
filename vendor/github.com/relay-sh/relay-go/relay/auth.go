package relay

import (
	"context"
	"errors"
)

type Credentials interface {
	RequestMetadata(ctx context.Context) (map[string]string, error)
}

type APIKey string

var ErrEmptyAPIKey = errors.New("relay: API key is empty")

func (k APIKey) RequestMetadata(context.Context) (map[string]string, error) {
	if k == "" {
		return nil, ErrEmptyAPIKey
	}
	return map[string]string{"authorization": "Bearer " + string(k)}, nil
}

type perRPC struct {
	creds      Credentials
	origin     string
	requireTLS bool
}

func (p perRPC) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	md, err := p.creds.RequestMetadata(ctx)
	if err != nil {
		return nil, err
	}
	if p.origin != "" {
		md["x-relay-origin"] = p.origin
	}
	return md, nil
}

func (p perRPC) RequireTransportSecurity() bool {
	return p.requireTLS
}
