package provider

import (
	"context"
	"errors"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

func waitDatabaseRunning(ctx context.Context, c *client.Client, id string, timeout time.Duration) (*serverv1.Database, error) {
	var db *serverv1.Database
	err := waitFor(ctx, timeout, func(ctx context.Context) (bool, error) {
		out, err := c.API.GetDatabase(ctx, &serverv1.GetDatabaseRequest{Id: id})
		if err != nil {
			return false, err
		}
		db = out.GetDatabase()
		switch db.GetStatus().GetPhase() {
		case phaseRunning:
			return !changing(db.GetStatus()), nil
		case phaseFailed:
			return false, errors.New("provisioning failed: " + db.GetStatus().GetMessage())
		}
		return false, nil
	})
	return db, err
}

func deleteDatabase(ctx context.Context, c *client.Client, id string, keepBackups bool, timeout time.Duration, diags *diag.Diagnostics) {
	_, err := c.API.DeleteDatabase(ctx, &serverv1.DeleteDatabaseRequest{Id: id, KeepBackups: keepBackups})
	if relay.IsNotFound(err) {
		return
	}
	if err != nil {
		diags.AddError("Cannot delete cluster", err.Error())
		return
	}
	err = waitFor(ctx, timeout, func(ctx context.Context) (bool, error) {
		out, err := c.API.GetDatabase(ctx, &serverv1.GetDatabaseRequest{Id: id})
		if relay.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		st := out.GetDatabase().GetStatus()
		if st.GetPhase() == phaseFailed {
			return false, errors.New("deletion failed: " + st.GetMessage())
		}
		return st.GetPhase() == phaseDeleted, nil
	})
	if err != nil {
		diags.AddError("Cluster was not deleted", err.Error())
	}
}

func findDatabaseByName(ctx context.Context, c *client.Client, name, engine string) (*serverv1.Database, error) {
	token := ""
	for {
		out, err := c.API.ListDatabases(ctx, &serverv1.ListDatabasesRequest{PageToken: token})
		if err != nil {
			return nil, err
		}
		for _, db := range out.GetDatabases() {
			if db.GetName() == name && db.GetSpec().GetEngine() == engine {
				return db, nil
			}
		}
		token = out.GetNextPageToken()
		if token == "" {
			return nil, nil
		}
	}
}
