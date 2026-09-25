package main

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func gitConnectionMessage(c workflow.GitConnection) *api.GitConnection {
	return &api.GitConnection{Id: c.ID, Host: c.Host, Username: c.Username,
		CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339Nano)}
}

func (s *workflowService) ListGitConnections(ctx context.Context, req *connect.Request[api.ListGitConnectionsRequest]) (*connect.Response[api.ListGitConnectionsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	// Only owners/admins attach organization Git connections to sources, so
	// only they need to enumerate them (host and account are not public).
	if caller.Role != "owner" && caller.Role != "admin" {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("git connections are admin-only"))
	}
	connections, err := s.store.ListGitConnections(ctx, caller.OrganizationID)
	if err != nil {
		return nil, workflowError(err)
	}
	response := &api.ListGitConnectionsResponse{}
	for _, c := range connections {
		response.Connections = append(response.Connections, gitConnectionMessage(c))
	}
	return connect.NewResponse(response), nil
}

func (s *workflowService) CreateGitConnection(ctx context.Context, req *connect.Request[api.CreateGitConnectionRequest]) (*connect.Response[api.CreateGitConnectionResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	token := []byte(req.Msg.Token)
	defer clear(token)
	c, err := s.store.CreateGitConnectionAs(ctx, caller, req.Msg.Host, req.Msg.Username, token, s.secrets)
	if err != nil {
		return nil, workflowError(err)
	}
	return connect.NewResponse(&api.CreateGitConnectionResponse{Connection: gitConnectionMessage(c)}), nil
}

// privateSourceCredential reads the private source's token for the platform's
// own admission fetch, only while the project's git.read grant is live.
func (s *workflowService) privateSourceCredential(ctx context.Context, orgID string, source workflow.ProjectSource) (string, []byte, error) {
	ctx = tenant.Org(ctx, orgID)
	if s.dispatcher == nil || s.dispatcher.DB == nil || s.secrets == nil {
		return "", nil, workflow.ErrGitConnection
	}
	db := s.dispatcher.DB
	if err := access.PreflightGitRead(ctx, db, orgID, source.ProjectID, source.GitConnectionID, source.RepositoryURL); err != nil {
		if errors.Is(err, access.ErrDenied) {
			return "", nil, workflow.ErrGitConnection
		}
		return "", nil, err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback(ctx)
	var username string
	if err := tx.QueryRow(ctx, `SELECT external_account_id FROM access_connections WHERE organization_id=$1 AND id=$2`,
		orgID, source.GitConnectionID).Scan(&username); err != nil {
		return "", nil, workflow.ErrGitConnection
	}
	secret, err := s.secrets.ReadCurrent(ctx, tx, orgID, source.GitConnectionID)
	if err != nil {
		return "", nil, workflow.ErrGitConnection
	}
	return username, secret.Bytes, nil
}
