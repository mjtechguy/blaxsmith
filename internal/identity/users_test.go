package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/netip"
	"sync"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func TestUserAdministrationPostgres(t *testing.T) {
	pool := identityTestPool(t)
	ctx := tenant.System(context.Background())
	ownerPassword := []byte("correct horse battery staple")
	if _, err := BootstrapOwner(ctx, pool, "alice@example.com", "engineering", "Engineering", ownerPassword); err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewSessionManager(pool, "blaxsmith-test", signer)
	if err != nil {
		t.Fatal(err)
	}
	users, err := NewUserAdmin(pool)
	if err != nil {
		t.Fatal(err)
	}
	source := netip.MustParseAddr("192.0.2.10")
	login := func(username string, password []byte) (Caller, Tokens, error) {
		t.Helper()
		tokens, err := manager.LoginLocal(ctx, "engineering", username+"@example.com", password, source, "")
		if err != nil {
			return Caller{}, Tokens{}, err
		}
		caller, err := manager.ValidateAccess(ctx, tokens.Access)
		if err != nil {
			t.Fatal(err)
		}
		return caller, tokens, nil
	}
	mustLogin := func(username string, password []byte) (Caller, Tokens) {
		t.Helper()
		caller, tokens, err := login(username, password)
		if err != nil {
			t.Fatalf("login %s: %v", username, err)
		}
		return caller, tokens
	}
	revoked := func(tokens Tokens) bool {
		t.Helper()
		_, err := manager.ValidateAccess(ctx, tokens.Access)
		return errors.Is(err, ErrUnauthenticated)
	}
	setup := func(link AccountLink, password string) {
		t.Helper()
		if _, err := users.CompleteLink(ctx, link.Token, []byte(password), "", ""); err != nil {
			t.Fatalf("complete %s link: %v", link.Purpose, err)
		}
	}
	owner, _ := mustLogin("alice", ownerPassword)

	// Invitation creates an invited member with a single-use setup link.
	bobLink, err := users.Invite(ctx, owner, "Bob@example.com", "Bob Builder", "admin")
	if err != nil || bobLink.Purpose != "setup" || len(bobLink.Token) != 43 {
		t.Fatalf("invite: %+v %v", bobLink, err)
	}
	if _, err := users.Invite(ctx, owner, "  BOB@Example.COM ", "", "member"); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("case-insensitive duplicate email: %v", err)
	}
	for _, bad := range []string{"x", "bob", "bob@localhost", "Bob <bob@example.com>", "a b@example.com", "bob@@example.com"} {
		if _, err := users.Invite(ctx, owner, bad, "", "member"); !errors.Is(err, ErrEmailInvalid) {
			t.Fatalf("invalid email %q: %v", bad, err)
		}
	}
	for _, bad := range [][3]string{{"valid@example.com", "", "superuser"}, {"valid@example.com", "bad\x00name", "member"}} {
		if _, err := users.Invite(ctx, owner, bad[0], bad[1], bad[2]); !errors.Is(err, ErrUserInvalid) {
			t.Fatalf("invalid invite %q: %v", bad, err)
		}
	}
	info, err := users.InspectLink(ctx, bobLink.Token)
	if err != nil || info.Username != "bob" || info.DisplayName != "Bob Builder" || info.OrganizationSlug != "engineering" || info.Purpose != "setup" {
		t.Fatalf("inspect link: %+v %v", info, err)
	}
	if _, _, err := login("bob", []byte("bob password 123")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("invited user signed in before setup: %v", err)
	}
	if _, err := users.CompleteLink(ctx, bobLink.Token, []byte("short"), "", ""); !errors.Is(err, ErrPassword) {
		t.Fatalf("short password: %v", err)
	}
	setup(bobLink, "bob password 123")
	if _, err := users.CompleteLink(ctx, bobLink.Token, []byte("bob password 456"), "", ""); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("setup link reused: %v", err)
	}
	if _, err := users.InspectLink(ctx, "not-a-token"); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("malformed token: %v", err)
	}
	admin, adminTokens := mustLogin("bob", []byte("bob password 123"))

	// Admins cannot create, grant, or touch owners.
	if _, err := users.Invite(ctx, admin, "mallory@example.com", "", "owner"); !errors.Is(err, ErrOwnerOnly) {
		t.Fatalf("admin invited owner: %v", err)
	}
	carolLink, err := users.Invite(ctx, admin, "carol@example.com", "", "member")
	if err != nil {
		t.Fatal(err)
	}
	setup(carolLink, "carol password 1")
	daveLink, err := users.Invite(ctx, admin, "dave@example.com", "", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	setup(daveLink, "dave password 12")
	member, memberTokens := mustLogin("carol", []byte("carol password 1"))
	viewer, viewerTokens := mustLogin("dave", []byte("dave password 12"))
	if err := users.SetRole(ctx, admin, member.PrincipalID, "owner"); !errors.Is(err, ErrOwnerOnly) {
		t.Fatalf("admin elevated to owner: %v", err)
	}
	if err := users.SetRole(ctx, admin, owner.PrincipalID, "admin"); !errors.Is(err, ErrOwnerOnly) {
		t.Fatalf("admin demoted owner: %v", err)
	}
	if err := users.SetEnabled(ctx, admin, owner.PrincipalID, false); !errors.Is(err, ErrOwnerOnly) {
		t.Fatalf("admin disabled owner: %v", err)
	}
	if _, err := users.IssueReset(ctx, admin, owner.PrincipalID); !errors.Is(err, ErrOwnerOnly) {
		t.Fatalf("admin reset owner: %v", err)
	}
	if _, err := users.RevokeSessions(ctx, admin, owner.PrincipalID); !errors.Is(err, ErrOwnerOnly) {
		t.Fatalf("admin revoked owner sessions: %v", err)
	}

	// Members and viewers are denied every operation.
	for _, caller := range []Caller{member, viewer} {
		if _, err := users.ListMembers(ctx, caller); !errors.Is(err, ErrUserAdminDenied) {
			t.Fatalf("%s listed members: %v", caller.Role, err)
		}
		if _, err := users.Invite(ctx, caller, "eve@example.com", "", "viewer"); !errors.Is(err, ErrUserAdminDenied) {
			t.Fatalf("%s invited: %v", caller.Role, err)
		}
		if err := users.SetRole(ctx, caller, caller.PrincipalID, "admin"); !errors.Is(err, ErrUserAdminDenied) {
			t.Fatalf("%s changed role: %v", caller.Role, err)
		}
		if err := users.SetEnabled(ctx, caller, admin.PrincipalID, false); !errors.Is(err, ErrUserAdminDenied) {
			t.Fatalf("%s disabled: %v", caller.Role, err)
		}
		if _, err := users.IssueReset(ctx, caller, admin.PrincipalID); !errors.Is(err, ErrUserAdminDenied) {
			t.Fatalf("%s issued reset: %v", caller.Role, err)
		}
		if _, err := users.RevokeSessions(ctx, caller, admin.PrincipalID); !errors.Is(err, ErrUserAdminDenied) {
			t.Fatalf("%s revoked sessions: %v", caller.Role, err)
		}
	}
	// A token that claims a role it no longer has is fenced.
	forged := member
	forged.Role = "admin"
	if _, err := users.Invite(ctx, forged, "eve@example.com", "", "viewer"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("forged role accepted: %v", err)
	}

	// The last owner who can sign in cannot be demoted or disabled, and an
	// invited owner without a password does not count.
	if err := users.SetRole(ctx, owner, owner.PrincipalID, "admin"); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("last owner demoted: %v", err)
	}
	if err := users.SetEnabled(ctx, owner, owner.PrincipalID, false); !errors.Is(err, ErrSelfDisable) {
		t.Fatalf("self disable: %v", err)
	}
	erinLink, err := users.Invite(ctx, owner, "erin@example.com", "Erin", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := users.SetRole(ctx, owner, owner.PrincipalID, "admin"); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demoted behind an invited owner: %v", err)
	}

	// Role changes and disabling revoke the member's sessions immediately.
	if err := users.SetRole(ctx, owner, member.PrincipalID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if !revoked(memberTokens) {
		t.Fatal("role change left the session live")
	}
	if _, err := manager.Refresh(ctx, memberTokens.Refresh); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("refresh after role change: %v", err)
	}
	if err := users.SetEnabled(ctx, owner, viewer.PrincipalID, false); err != nil {
		t.Fatal(err)
	}
	if !revoked(viewerTokens) {
		t.Fatal("disable left the session live")
	}
	if _, _, err := login("dave", []byte("dave password 12")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("disabled member signed in: %v", err)
	}
	if _, err := users.IssueReset(ctx, owner, viewer.PrincipalID); !errors.Is(err, ErrUserInvalid) {
		t.Fatalf("reset for disabled member: %v", err)
	}
	if err := users.SetEnabled(ctx, owner, viewer.PrincipalID, true); err != nil {
		t.Fatal(err)
	}
	mustLogin("dave", []byte("dave password 12"))

	// Revoke sessions signs a member out everywhere in the organization.
	count, err := users.RevokeSessions(ctx, owner, admin.PrincipalID)
	if err != nil || count != 1 || !revoked(adminTokens) {
		t.Fatalf("revoke sessions: %d %v", count, err)
	}
	if _, err := users.Invite(ctx, admin, "eve@example.com", "", "viewer"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked admin session still mutates: %v", err)
	}

	// Reset links are single-use, superseded by a newer link, and expire.
	_, carolTokens := mustLogin("carol", []byte("carol password 1"))
	first, err := users.IssueReset(ctx, owner, member.PrincipalID)
	if err != nil || first.Purpose != "reset" {
		t.Fatalf("issue reset: %+v %v", first, err)
	}
	second, err := users.IssueReset(ctx, owner, member.PrincipalID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := users.InspectLink(ctx, first.Token); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("superseded link usable: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE identity_account_links SET created_at=clock_timestamp()-interval '2 days',
		expires_at=clock_timestamp()-interval '1 second' WHERE principal_id=$1 AND consumed_at IS NULL AND revoked_at IS NULL`,
		member.PrincipalID); err != nil {
		t.Fatal(err)
	}
	if _, err := users.CompleteLink(ctx, second.Token, []byte("carol password 2"), "", ""); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("expired link used: %v", err)
	}
	third, err := users.IssueReset(ctx, owner, member.PrincipalID)
	if err != nil {
		t.Fatal(err)
	}
	setup(third, "carol password 2")
	if !revoked(carolTokens) {
		t.Fatal("password reset left a session live")
	}
	if _, _, err := login("carol", []byte("carol password 1")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("old password still works: %v", err)
	}
	mustLogin("carol", []byte("carol password 2"))
	// An invited member's reset is a fresh setup link.
	erinAgain, err := users.IssueReset(ctx, owner, erinLink.PrincipalID)
	if err != nil || erinAgain.Purpose != "setup" {
		t.Fatalf("reissue setup: %+v %v", erinAgain, err)
	}
	if _, err := users.InspectLink(ctx, erinLink.Token); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("old setup link still open: %v", err)
	}

	// Other organizations: members are invisible, and a shared account's
	// installation-wide password cannot be reset from one organization.
	var otherOrg string
	if err := pool.QueryRow(ctx, `INSERT INTO identity_organizations (id,slug,name) VALUES (gen_random_uuid(),'research','Research') RETURNING id`).
		Scan(&otherOrg); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity_memberships (organization_id,principal_id,role) VALUES ($1,$2,'member')`,
		otherOrg, member.PrincipalID); err != nil {
		t.Fatal(err)
	}
	if _, err := users.IssueReset(ctx, owner, member.PrincipalID); !errors.Is(err, ErrSharedAccount) {
		t.Fatalf("shared account reset: %v", err)
	}
	var outsiderID, outsiderSession string
	if err := pool.QueryRow(ctx, `INSERT INTO identity_principals (id,username,password_hash) VALUES (gen_random_uuid(),'olga','x') RETURNING id`).
		Scan(&outsiderID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity_memberships (organization_id,principal_id,role) VALUES ($1,$2,'owner')`, otherOrg, outsiderID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO identity_sessions (organization_id,id,principal_id,auth_method,mfa_level,expires_at)
		VALUES ($1,gen_random_uuid(),$2,'local','none',clock_timestamp()+interval '1 hour') RETURNING id`, otherOrg, outsiderID).
		Scan(&outsiderSession); err != nil {
		t.Fatal(err)
	}
	outsider := Caller{OrganizationID: otherOrg, PrincipalID: outsiderID, SessionID: outsiderSession, Role: "owner",
		AccessExpires: owner.AccessExpires}
	if err := users.SetRole(ctx, outsider, admin.PrincipalID, "viewer"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("cross-org role change: %v", err)
	}
	if err := users.SetEnabled(ctx, outsider, owner.PrincipalID, false); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("cross-org disable: %v", err)
	}
	// Inviting an address held only in another organization reveals nothing
	// beyond an invalid address; one held by an own member says so.
	if _, err := users.Invite(ctx, outsider, "bob@example.com", "", "member"); !errors.Is(err, ErrEmailInvalid) {
		t.Fatalf("cross-org email revealed: %v", err)
	}
	var sharedEmail string
	if err := pool.QueryRow(ctx, `SELECT email FROM identity_principals WHERE id=$1`, member.PrincipalID).Scan(&sharedEmail); err != nil {
		t.Fatal(err)
	}
	if _, err := users.Invite(ctx, outsider, sharedEmail, "", "member"); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("own member's email: %v", err)
	}

	members, err := users.ListMembers(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, m := range members {
		status[m.Username] = m.Role + "/" + m.Status
	}
	if len(members) != 5 || status["alice"] != "owner/active" || status["erin"] != "owner/invited" ||
		status["bob"] != "admin/active" || status["carol"] != "viewer/active" || status["dave"] != "viewer/active" ||
		members[0].LastLogin == nil {
		t.Fatalf("members: %v", status)
	}
	var audited int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_audit_events WHERE organization_id=(SELECT id FROM identity_organizations WHERE slug='engineering')
		AND (action LIKE 'identity.user.%' OR action LIKE 'identity.account.%')`).Scan(&audited); err != nil || audited != 16 {
		t.Fatalf("audited %d: %v", audited, err)
	}
	var detail string
	if err := pool.QueryRow(ctx, `SELECT detail::text FROM identity_audit_events WHERE action='identity.user.role_changed'
		AND subject_id=$1`, member.PrincipalID).Scan(&detail); err != nil || detail != `{"to": "viewer", "from": "member"}` {
		t.Fatalf("role audit detail %q: %v", detail, err)
	}

	// Two owners demoting each other at once still leave an owner.
	setup(erinAgain, "erin password 12")
	erin, _ := mustLogin("erin", []byte("erin password 12"))
	var wg sync.WaitGroup
	for _, pair := range [][2]Caller{{owner, erin}, {erin, owner}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = users.SetRole(ctx, pair[0], pair[1].PrincipalID, "admin")
		}()
	}
	wg.Wait()
	var owners int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_memberships m JOIN identity_organizations o ON o.id=m.organization_id
		WHERE o.slug='engineering' AND m.role='owner'`).Scan(&owners); err != nil || owners < 1 {
		t.Fatalf("owners after race: %d %v", owners, err)
	}
}
