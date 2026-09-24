package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1/apiv1connect"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

func TestAccountErrorCodes(t *testing.T) {
	for err, want := range map[error]connect.Code{
		identity.ErrRateLimited:     connect.CodeResourceExhausted,
		identity.ErrCurrentPassword: connect.CodePermissionDenied,
		identity.ErrEmailTaken:      connect.CodeAlreadyExists,
		identity.ErrEmailRequired:   connect.CodeFailedPrecondition,
		identity.ErrCurrentSession:  connect.CodeFailedPrecondition,
		identity.ErrSessionNotFound: connect.CodeNotFound,
		identity.ErrEmailInvalid:    connect.CodeInvalidArgument,
		identity.ErrPassword:        connect.CodeInvalidArgument,
		identity.ErrUnauthenticated: connect.CodeUnauthenticated,
		errors.New("boom"):          connect.CodeInternal,
	} {
		if got := connect.CodeOf(accountError(err)); got != want {
			t.Fatalf("%v mapped to %v, want %v", err, got, want)
		}
	}
}

// browser is one cookie jar signed in (or not) against the HTTPS app.
type browser struct {
	t       *testing.T
	ctx     context.Context
	origin  string
	csrf    string
	auth    apiv1connect.AuthServiceClient
	account apiv1connect.AccountServiceClient
	users   apiv1connect.UserAdminServiceClient
	home    apiv1connect.WorkspaceServiceClient
}

func newBrowser(t *testing.T, ctx context.Context, transport http.RoundTripper, origin string) *browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: transport, Jar: jar}
	b := &browser{t: t, ctx: ctx, origin: origin, auth: apiv1connect.NewAuthServiceClient(client, origin+"/api"),
		account: apiv1connect.NewAccountServiceClient(client, origin+"/api"),
		users:   apiv1connect.NewUserAdminServiceClient(client, origin+"/api"),
		home:    apiv1connect.NewWorkspaceServiceClient(client, origin+"/api")}
	csrf, err := b.auth.GetCsrf(ctx, read(b, &api.GetCsrfRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	b.csrf = csrf.Msg.Token
	return b
}

func read[T any](b *browser, msg *T) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Origin", b.origin)
	return req
}

func write[T any](b *browser, msg *T) *connect.Request[T] {
	req := read(b, msg)
	req.Header().Set("X-Blaxsmith-CSRF", b.csrf)
	return req
}

func (b *browser) login(slug, login, password string) (*api.SessionIdentity, error) {
	got, err := b.auth.LoginLocal(b.ctx, write(b, &api.LoginLocalRequest{OrganizationSlug: slug, Email: login, Password: password}))
	if err != nil {
		return nil, err
	}
	return got.Msg.Session, nil
}

func (b *browser) mustLogin(slug, login, password string) *api.SessionIdentity {
	b.t.Helper()
	session, err := b.login(slug, login, password)
	if err != nil {
		b.t.Fatalf("login %s: %v", login, err)
	}
	return session
}

func (b *browser) signedIn() bool {
	_, err := b.auth.CurrentSession(b.ctx, read(b, &api.CurrentSessionRequest{}))
	return err == nil
}

func (b *browser) homeErr() error {
	_, err := b.home.GetWorkspaceHome(b.ctx, read(b, &api.GetWorkspaceHomeRequest{}))
	return err
}

func strptr(s string) *string { return &s }

// testAccountBrowserAPI drives email sign-in, invite setup, the one-time
// legacy username path, AccountService, and admin email repair through the
// real HTTPS handler. The owner belongs to two organizations by now.
func testAccountBrowserAPI(t *testing.T, ctx context.Context, pool *pgxpool.Pool, transport http.RoundTripper, origin string, password []byte) {
	t.Helper()
	code := func(err error) connect.Code { return connect.CodeOf(err) }
	// The workflow checks leave the owner demoted to viewer; restore it.
	if _, err := pool.Exec(ctx, `UPDATE identity_memberships SET role='owner' WHERE organization_id=
		(SELECT id FROM identity_organizations WHERE slug='engineering')
		AND principal_id=(SELECT id FROM identity_principals WHERE email='alice@example.com')`); err != nil {
		t.Fatal(err)
	}

	// Sign-in by email: case-insensitive, no enumeration, organization asked
	// for only after a correct password.
	owner := newBrowser(t, ctx, transport, origin)
	_, wrong := owner.login("engineering", "alice@example.com", "not the right password")
	_, unknown := owner.login("engineering", "nobody@example.com", string(password))
	if code(wrong) != connect.CodeUnauthenticated || code(unknown) != connect.CodeUnauthenticated ||
		wrong.Error() != unknown.Error() {
		t.Fatalf("login failures differ: %v / %v", wrong, unknown)
	}
	if _, err := owner.login("", "ALICE@example.com", string(password)); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("multi-organization login without an organization: %v", err)
	}
	ownerSession := owner.mustLogin("engineering", "ALICE@Example.com", string(password))
	if ownerSession.EmailRequired {
		t.Fatal("email login flagged email_required")
	}

	// Invite by email; setup shows it and takes a display name.
	invite, err := owner.users.InviteUser(ctx, write(owner, &api.InviteUserRequest{Email: "Carol@Example.com", Role: "member"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.users.InviteUser(ctx, write(owner, &api.InviteUserRequest{Email: "carol@EXAMPLE.com", Role: "member"})); code(err) != connect.CodeAlreadyExists {
		t.Fatalf("duplicate email invite: %v", err)
	}
	if _, err := owner.users.InviteUser(ctx, write(owner, &api.InviteUserRequest{Email: "carol", Role: "member"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid email invite: %v", err)
	}
	carol := newBrowser(t, ctx, transport, origin)
	link, err := carol.users.GetAccountLink(ctx, read(carol, &api.GetAccountLinkRequest{Token: invite.Msg.Link.Token}))
	if err != nil || link.Msg.Email != "carol@example.com" {
		t.Fatalf("setup link: %+v %v", link, err)
	}
	if _, err := carol.users.CompleteAccountLink(ctx, write(carol, &api.CompleteAccountLinkRequest{Token: invite.Msg.Link.Token,
		Password: "short"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("weak setup password: %v", err)
	}
	done, err := carol.users.CompleteAccountLink(ctx, write(carol, &api.CompleteAccountLinkRequest{Token: invite.Msg.Link.Token,
		Password: "carol password 123", DisplayName: "Carol Chen"}))
	if err != nil || done.Msg.Email != "carol@example.com" {
		t.Fatalf("complete setup: %+v %v", done, err)
	}
	carol.mustLogin("", "carol@example.com", "carol password 123")
	profile, err := carol.account.GetMyProfile(ctx, read(carol, &api.GetMyProfileRequest{}))
	if err != nil || profile.Msg.Profile.DisplayName != "Carol Chen" || profile.Msg.Profile.Email != "carol@example.com" ||
		profile.Msg.Profile.Role != "member" || profile.Msg.Profile.EmailRequired {
		t.Fatalf("profile: %+v %v", profile, err)
	}

	// A legacy account signs in by username once and must set an email before
	// any guarded RPC works.
	hash, err := identity.HashPassword([]byte("legacy password 1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `WITH p AS (INSERT INTO identity_principals (id,username,password_hash)
		VALUES (gen_random_uuid(),'legacy',$1) RETURNING id)
		INSERT INTO identity_memberships (organization_id,principal_id,role)
		SELECT o.id,p.id,'member' FROM p, identity_organizations o WHERE o.slug='engineering'`, hash); err != nil {
		t.Fatal(err)
	}
	legacy := newBrowser(t, ctx, transport, origin)
	if session := legacy.mustLogin("", "legacy", "legacy password 1"); !session.EmailRequired {
		t.Fatal("legacy login not flagged")
	}
	current, err := legacy.auth.CurrentSession(ctx, read(legacy, &api.CurrentSessionRequest{}))
	if err != nil || !current.Msg.Session.EmailRequired {
		t.Fatalf("current session: %+v %v", current, err)
	}
	if got := code(legacy.homeErr()); got != connect.CodeFailedPrecondition {
		t.Fatalf("guarded RPC during email_required: %v", got)
	}
	if _, err := legacy.account.ChangeMyPassword(ctx, write(legacy, &api.ChangeMyPasswordRequest{CurrentPassword: "legacy password 1",
		NewPassword: "legacy password 2"})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("password change during email_required: %v", err)
	}
	if _, err := legacy.account.UpdateMyProfile(ctx, write(legacy, &api.UpdateMyProfileRequest{DisplayName: strptr("Leggy")})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("name change during email_required: %v", err)
	}
	if _, err := newBrowser(t, ctx, transport, origin).login("", "legacy", "legacy password 1"); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("second legacy login: %v", err)
	}
	set, err := legacy.account.UpdateMyProfile(ctx, write(legacy, &api.UpdateMyProfileRequest{Email: strptr("Legacy@Example.com"),
		CurrentPassword: "legacy password 1"}))
	if err != nil || set.Msg.Profile.Email != "legacy@example.com" || set.Msg.Profile.EmailRequired || set.Msg.Profile.EmailVerified {
		t.Fatalf("set email: %+v %v", set, err)
	}
	if got := legacy.homeErr(); got != nil {
		t.Fatalf("guarded RPC after email set: %v", got)
	}

	// Own account changes: CSRF, current password, strength, other sessions.
	carolLaptop := newBrowser(t, ctx, transport, origin)
	carolLaptop.mustLogin("", "carol@example.com", "carol password 123")
	noCSRF := read(carol, &api.ChangeMyPasswordRequest{CurrentPassword: "carol password 123", NewPassword: "carol password 456"})
	if _, err := carol.account.ChangeMyPassword(ctx, noCSRF); code(err) != connect.CodePermissionDenied {
		t.Fatalf("password change without CSRF: %v", err)
	}
	if _, err := carol.account.ChangeMyPassword(ctx, write(carol, &api.ChangeMyPasswordRequest{CurrentPassword: "wrong password 12",
		NewPassword: "carol password 456"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("wrong current password: %v", err)
	}
	if _, err := carol.account.ChangeMyPassword(ctx, write(carol, &api.ChangeMyPasswordRequest{CurrentPassword: "carol password 123",
		NewPassword: "tiny"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("weak new password: %v", err)
	}
	if _, err := carol.account.UpdateMyProfile(ctx, write(carol, &api.UpdateMyProfileRequest{Email: strptr("legacy@example.com"),
		CurrentPassword: "carol password 123"})); code(err) != connect.CodeAlreadyExists {
		t.Fatalf("email change to a used email: %v", err)
	}
	changed, err := carol.account.UpdateMyProfile(ctx, write(carol, &api.UpdateMyProfileRequest{Email: strptr("carol@example.org"),
		CurrentPassword: "carol password 123"}))
	if err != nil || changed.Msg.RevokedSessions != 1 || changed.Msg.Profile.Email != "carol@example.org" {
		t.Fatalf("email change: %+v %v", changed, err)
	}
	if !carol.signedIn() || carolLaptop.signedIn() {
		t.Fatal("email change did not keep this session and end the other")
	}

	// Sessions are the caller's own only.
	ownerSessions, err := owner.account.ListMySessions(ctx, read(owner, &api.ListMySessionsRequest{}))
	if err != nil || len(ownerSessions.Msg.Sessions) == 0 {
		t.Fatalf("owner sessions: %+v %v", ownerSessions, err)
	}
	var ownerCurrent string
	for _, s := range ownerSessions.Msg.Sessions {
		if s.Current {
			ownerCurrent = s.Id
		}
	}
	if ownerCurrent == "" || ownerSessions.Msg.Sessions[0].UserAgent == "" {
		t.Fatalf("owner current session: %+v", ownerSessions.Msg.Sessions)
	}
	if _, err := carol.account.RevokeMySession(ctx, write(carol, &api.RevokeMySessionRequest{SessionId: ownerCurrent})); code(err) != connect.CodeNotFound {
		t.Fatalf("revoked another user's session: %v", err)
	}
	if !owner.signedIn() {
		t.Fatal("another user's session ended")
	}
	carolLaptop.mustLogin("", "carol@example.org", "carol password 123")
	carolSessions, err := carol.account.ListMySessions(ctx, read(carol, &api.ListMySessionsRequest{}))
	if err != nil || len(carolSessions.Msg.Sessions) != 2 {
		t.Fatalf("carol sessions: %+v %v", carolSessions, err)
	}
	for _, s := range carolSessions.Msg.Sessions {
		if !s.Current {
			if _, err := carol.account.RevokeMySession(ctx, write(carol, &api.RevokeMySessionRequest{SessionId: s.Id})); err != nil {
				t.Fatal(err)
			}
		} else if _, err := carol.account.RevokeMySession(ctx, write(carol, &api.RevokeMySessionRequest{SessionId: s.Id})); code(err) != connect.CodeFailedPrecondition {
			t.Fatalf("revoked the current session: %v", err)
		}
	}
	if carolLaptop.signedIn() {
		t.Fatal("revoked session still signed in")
	}
	if revoked, err := carol.account.ChangeMyPassword(ctx, write(carol, &api.ChangeMyPasswordRequest{CurrentPassword: "carol password 123",
		NewPassword: "carol password 456"})); err != nil || revoked.Msg.RevokedSessions != 0 {
		t.Fatalf("password change: %+v %v", revoked, err)
	}

	// Admin email repair: owners/admins only; revokes the member's sessions.
	carolID := changed.Msg.Profile.PrincipalId
	if _, err := carol.users.SetUserEmail(ctx, write(carol, &api.SetUserEmailRequest{PrincipalId: carolID, Email: "c@example.com"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("member set an email: %v", err)
	}
	if _, err := owner.users.SetUserEmail(ctx, write(owner, &api.SetUserEmailRequest{PrincipalId: carolID, Email: "Carol.Chen@Example.com"})); err != nil {
		t.Fatal(err)
	}
	if carol.signedIn() {
		t.Fatal("admin email change kept the member's session")
	}
	carol.mustLogin("", "carol.chen@example.com", "carol password 456")
	page, err := owner.home.ListMembersPage(ctx, read(owner, &api.ListMembersPageRequest{Search: "carol.chen@"}))
	if err != nil || len(page.Msg.Members) != 1 || page.Msg.Members[0].Email != "carol.chen@example.com" || page.Msg.Members[0].DisplayName != "Carol Chen" {
		t.Fatalf("search members by email: %+v %v", page, err)
	}

	for action, want := range map[string]int{
		"identity.login_legacy_username":    1,
		"identity.account.email_changed":    2,
		"identity.account.password_changed": 1,
		"identity.account.session_revoked":  1,
		"identity.user.email_changed":       1,
		"identity.account.setup_completed":  1,
		"identity.user.invited":             1,
	} {
		var got int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_audit_events e JOIN identity_principals p ON p.id=e.subject_id
			WHERE e.action=$1 AND p.email IN ('carol.chen@example.com','legacy@example.com')`, action).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if action == "identity.login_legacy_username" || action == "identity.account.session_revoked" {
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_audit_events WHERE action=$1
				AND actor_id IN (SELECT id FROM identity_principals WHERE email IN ('carol.chen@example.com','legacy@example.com'))`, action).Scan(&got); err != nil {
				t.Fatal(err)
			}
		}
		if got != want {
			t.Fatalf("audit %s: %d, want %d", action, got, want)
		}
	}
}
