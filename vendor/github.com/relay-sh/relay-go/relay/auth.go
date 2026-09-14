package relay

import "context"

type tokenAuth struct {
	token      string
	origin     string
	requireTLS bool
}

func (t tokenAuth) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	md := map[string]string{"authorization": "Bearer " + t.token}
	if t.origin != "" {
		md["x-relay-origin"] = t.origin
	}
	return md, nil
}

func (t tokenAuth) RequireTransportSecurity() bool {
	return t.requireTLS
}
