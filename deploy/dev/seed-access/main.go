// Command seed-access creates one synthetic private-Git authority chain.
// It is an operator-only dev fixture, not an account-onboarding API.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/access"
)

func main() {
	var space, task, repo, commit, tokenFile, modelTokenFile, keyFile, database string
	flag.StringVar(&space, "space", "", "synthetic AX atespace")
	flag.StringVar(&task, "task", "", "synthetic AX task")
	flag.StringVar(&repo, "repo", "", "private HTTPS Git repository")
	flag.StringVar(&commit, "commit", "", "frozen Git commit")
	flag.StringVar(&tokenFile, "token-file", "", "owner-only synthetic fixture token")
	flag.StringVar(&modelTokenFile, "model-token-file", "", "owner-only synthetic provider key")
	flag.StringVar(&keyFile, "key-file", "", "owner-only 32-byte database encryption key")
	flag.StringVar(&database, "database", "host=/var/run/postgresql user=root dbname=blaxsmith_dev sslmode=disable", "dev PostgreSQL connection")
	flag.Parse()
	if err := run(space, task, repo, commit, tokenFile, modelTokenFile, keyFile, database); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(space, task, repo, commit, tokenFile, modelTokenFile, keyFile, database string) error {
	u, err := url.Parse(repo)
	gitInput := repo != ""
	if space == "" || task == "" || keyFile == "" || (!gitInput && tokenFile != "") ||
		(gitInput && (err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Path == "" ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
			(len(commit) != 40 && len(commit) != 64) || tokenFile == "")) ||
		(!gitInput && modelTokenFile == "") || (modelTokenFile != "" && keyFile == "") {
		return errors.New("incomplete synthetic access seed")
	}
	readOwnerOnly := func(path string) ([]byte, error) {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, access.ErrDenied
		}
		return os.ReadFile(path)
	}
	key, err := readOwnerOnly(keyFile)
	if err != nil || len(key) != 32 {
		return access.ErrDenied
	}
	defer clear(key)
	var token, modelToken []byte
	if tokenFile != "" {
		token, err = readOwnerOnly(tokenFile)
		if err != nil || len(token) == 0 || len(token) > 8192 || strings.ContainsAny(string(token), "\r\n\x00") {
			return access.ErrDenied
		}
		defer clear(token)
	}
	if modelTokenFile != "" {
		modelToken, err = readOwnerOnly(modelTokenFile)
		if err != nil || !strings.HasPrefix(string(modelToken), "synthetic-model-") ||
			len(modelToken) > 8192 || strings.ContainsAny(string(modelToken), "\r\n\x00") {
			return access.ErrDenied
		}
		defer clear(modelToken)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := pgxpool.New(ctx, database)
	if err != nil {
		return err
	}
	defer db.Close()
	const organization = "synthetic-org"
	connectionID, grantID := space+"/private-git", space+"/git-read"
	bindingID := space + "/" + task + "/git-read"
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO access_project_policies
		(organization_id,project_id,version,git_read_enabled,delivery_modes)
		VALUES ($1,$2,1,$3,ARRAY['native_raw'])`, organization, space, gitInput); err != nil {
		return err
	}
	if gitInput {
		if _, err := tx.Exec(ctx, `INSERT INTO access_provider_registrations
			(organization_id,id,provider_kind,origin,delivery_modes,state)
			VALUES ($1,$2,'git',$3,ARRAY['native_raw'],'active')`,
			organization, space+"/provider", "https://"+strings.ToLower(u.Host)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO access_connections
			(organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state)
			VALUES ($1,$2,'organization',$1,$3,'synthetic-fixture-account','synthetic-basic','active')`,
			organization, connectionID, space+"/provider"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO access_grants
			(organization_id,id,connection_id,project_id,grantee_kind,grantee_id,
			 capability,resource,delivery_mode,issuer_id)
			VALUES ($1,$2,$3,$4,'user','synthetic-operator','git.read',$5,'native_raw','synthetic-operator')`,
			organization, grantID, connectionID, space, repo); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO access_bindings
			(organization_id,id,attempt_id,project_id,grant_id,grant_version,
			 capability,resource,input_commit,policy_version)
			VALUES ($1,$2,$3,$4,$5,1,'git.read',$6,$7,1)`,
			organization, bindingID, space+"/"+task, space, grantID, repo, commit); err != nil {
			return err
		}
	}
	modelConnection, modelGrantID := space+"/openai", space+"/model-invoke"
	if modelTokenFile != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO access_provider_registrations
			(organization_id,id,provider_kind,origin,delivery_modes,state)
			VALUES ($1,$2,'openai','https://api.openai.com',ARRAY['native_raw'],'active')`,
			organization, space+"/openai-provider"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO access_connections
			(organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state)
			VALUES ($1,$2,'organization',$1,$3,'synthetic-openai-account','api_key','active')`,
			organization, modelConnection, space+"/openai-provider"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO access_grants
			(organization_id,id,connection_id,project_id,grantee_kind,grantee_id,
			 capability,resource,delivery_mode,issuer_id,expires_at)
			VALUES ($1,$2,$3,$4,'user','synthetic-operator','model.invoke',
			 'openai/gpt-6-luna','native_raw','synthetic-operator',clock_timestamp()+interval '1 hour')`,
			organization, modelGrantID, modelConnection, space); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	store, err := access.NewSecretStore(db, "dev-key-1", map[string][]byte{"dev-key-1": key})
	if err != nil {
		return err
	}
	out := map[string]any{"organization_id": organization, "project_id": space}
	if gitInput {
		version, err := store.Rotate(ctx, organization, connectionID, 0, token, nil)
		if err != nil {
			return err
		}
		out["binding_id"], out["secret_version"] = bindingID, version
	}
	if modelTokenFile != "" {
		version, err := store.Rotate(ctx, organization, modelConnection, 0, modelToken, nil)
		if err != nil {
			return err
		}
		model := access.ModelGrant{OrganizationID: organization, ProjectID: space, GrantID: modelGrantID,
			GranteeKind: "user", GranteeID: "synthetic-operator", Provider: "openai", Model: "gpt-6-luna"}
		bind, err := db.Begin(ctx)
		if err != nil {
			return err
		}
		defer bind.Rollback(ctx)
		approval, err := access.PreflightModelInvoke(ctx, bind, model)
		if err != nil {
			return err
		}
		modelBinding, err := access.BindModelInvoke(ctx, bind, model, approval, space+"/"+task)
		if err != nil {
			return err
		}
		if err := bind.Commit(ctx); err != nil {
			return err
		}
		out["model_binding_id"], out["model_secret_version"] = modelBinding, version
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}
