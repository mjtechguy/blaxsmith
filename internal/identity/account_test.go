package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/netip"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func TestEmailIdentityPostgres(t *testing.T) {
	pool := identityTestPool(t)
	ctx := tenant.System(context.Background())
	password := []byte("correct horse battery staple")
	owner, err := BootstrapOwner(ctx, pool, "  Alice@Example.COM ", "engineering", "Engineering", password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BootstrapOwner(ctx, pool, "not-an-email", "other", "Other", password); !errors.Is(err, ErrInvalidOwner) {
		t.Fatalf("bootstrap without an email: %v", err)
	}
	var email, handle string
	if err := pool.QueryRow(ctx, `SELECT email,username FROM identity_principals WHERE id=$1`, owner.PrincipalID).
		Scan(&email, &handle); err != nil || email != "alice@example.com" || handle != "alice" {
		t.Fatalf("owner stored as %q/%q: %v", email, handle, err)
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
	next := byte(0)
	source := func() netip.Addr { next++; return netip.AddrFrom4([4]byte{198, 51, 100, next}) }
	login := func(slug, login string, pw []byte) (Tokens, Caller, error) {
		t.Helper()
		tokens, err := manager.LoginLocal(ctx, slug, login, pw, source(), "Test Browser/1.0")
		if err != nil {
			return Tokens{}, Caller{}, err
		}
		caller, err := manager.ValidateAccess(ctx, tokens.Access)
		if err != nil {
			t.Fatal(err)
		}
		return tokens, caller, nil
	}
	mustLogin := func(slug, name string, pw []byte) (Tokens, Caller) {
		t.Helper()
		tokens, caller, err := login(slug, name, pw)
		if err != nil {
			t.Fatalf("login %s: %v", name, err)
		}
		return tokens, caller
	}
	live := func(tokens Tokens) bool {
		t.Helper()
		_, err := manager.ValidateAccess(ctx, tokens.Access)
		return err == nil
	}
	actions := func(action string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_audit_events WHERE action=$1`, action).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Email sign-in is case-insensitive, needs no organization for a single
	// membership, and fails identically for every wrong combination.
	aliceTokens, alice := mustLogin("", "ALICE@example.com", password)
	if aliceTokens.EmailRequired || alice.EmailRequired || alice.Role != "owner" {
		t.Fatalf("email login: %+v", alice)
	}
	for _, attempt := range []struct{ slug, login, password string }{
		{"", "alice@example.com", "wrong password here"},
		{"", "nobody@example.com", string(password)},
		{"", "alice", string(password)}, // Has an email: the handle is not a login.
		{"other-org", "alice@example.com", string(password)},
		{"", "", string(password)},
	} {
		if _, _, err := login(attempt.slug, attempt.login, []byte(attempt.password)); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("login %+v: %v", attempt, err)
		}
	}

	// Invite by email; the setup link shows the email and sets the name.
	link, err := users.Invite(ctx, alice, "Bob.Builder+ci@Example.com", "", "member")
	if err != nil {
		t.Fatal(err)
	}
	info, err := users.InspectLink(ctx, link.Token)
	if err != nil || info.Email != "bob.builder+ci@example.com" || info.Username != "bob.builder-ci" {
		t.Fatalf("invite link: %+v %v", info, err)
	}
	if _, _, err := login("", "bob.builder+ci@example.com", []byte("bob password 123")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("invited account signed in before setup: %v", err)
	}
	done, err := users.CompleteLink(ctx, link.Token, []byte("bob password 123"), " Bob Builder ", "ignored@example.com")
	if err != nil || done.Email != "bob.builder+ci@example.com" || done.DisplayName != "Bob Builder" {
		t.Fatalf("complete setup: %+v %v", done, err)
	}
	bobTokens, bob := mustLogin("engineering", "Bob.Builder+CI@example.com", []byte("bob password 123"))
	if _, err := users.Invite(ctx, alice, "bob.builder@example.com", "", "member"); err != nil {
		t.Fatalf("second invite with a colliding handle: %v", err)
	}
	var suffixed string
	if err := pool.QueryRow(ctx, `SELECT username FROM identity_principals WHERE email='bob.builder@example.com'`).Scan(&suffixed); err != nil ||
		suffixed != "bob.builder" {
		t.Fatalf("handle: %q %v", suffixed, err)
	}
	if _, err := users.Invite(ctx, alice, "bob.builder+ci@example.org", "", "member"); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT username FROM identity_principals WHERE email='bob.builder+ci@example.org'`).Scan(&suffixed); err != nil ||
		suffixed != "bob.builder-ci-2" {
		t.Fatalf("handle: %q %v", suffixed, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity_principals (id,username,email) VALUES (gen_random_uuid(),'shouty','ALICE@EXAMPLE.COM')`); err == nil {
		t.Fatal("database accepted a non-normalized email")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity_principals (id,username,email) VALUES (gen_random_uuid(),'twin','alice@example.com')`); err == nil {
		t.Fatal("database accepted a duplicate email")
	}

	// A principal from before emails signs in with its username exactly once;
	// that session can only set an email.
	hash, err := HashPassword([]byte("olga password 12"))
	if err != nil {
		t.Fatal(err)
	}
	var olgaID string
	if err := pool.QueryRow(ctx, `WITH p AS (INSERT INTO identity_principals (id,username,password_hash)
		VALUES (gen_random_uuid(),'olga',$2) RETURNING id)
		INSERT INTO identity_memberships (organization_id,principal_id,role) SELECT $1,id,'member' FROM p RETURNING principal_id`,
		owner.OrganizationID, hash).Scan(&olgaID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := login("", "olga", []byte("wrong password 12")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("legacy wrong password: %v", err)
	}
	olgaTokens, olga := mustLogin("", "OLGA", []byte("olga password 12"))
	if !olgaTokens.EmailRequired || !olga.EmailRequired {
		t.Fatalf("legacy session not flagged: %+v", olga)
	}
	if _, _, err := login("", "olga", []byte("olga password 12")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("legacy username accepted twice: %v", err)
	}
	refreshed, err := manager.Refresh(ctx, olgaTokens.Refresh)
	if err != nil || !refreshed.EmailRequired {
		t.Fatalf("refresh dropped email_required: %+v %v", refreshed, err)
	}
	olga, _ = manager.ValidateAccess(ctx, refreshed.Access)
	profile, err := manager.Profile(ctx, olga)
	if err != nil || !profile.EmailRequired || profile.Email != "" || profile.Handle != "olga" {
		t.Fatalf("legacy profile: %+v %v", profile, err)
	}
	name := "Olga"
	if _, _, err := manager.UpdateProfile(ctx, olga, ProfileChange{DisplayName: &name}); !errors.Is(err, ErrEmailRequired) {
		t.Fatalf("legacy session changed its name first: %v", err)
	}
	taken := "ALICE@example.com"
	if _, _, err := manager.UpdateProfile(ctx, olga, ProfileChange{Email: &taken, CurrentPassword: []byte("olga password 12")}); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("legacy session took a used email: %v", err)
	}
	olgaEmail := "olga@example.com"
	if _, _, err := manager.UpdateProfile(ctx, olga, ProfileChange{Email: &olgaEmail, CurrentPassword: []byte("wrong password 12")}); !errors.Is(err, ErrCurrentPassword) {
		t.Fatalf("email set with a wrong password: %v", err)
	}
	profile, _, err = manager.UpdateProfile(ctx, olga, ProfileChange{Email: &olgaEmail, DisplayName: &name, CurrentPassword: []byte("olga password 12")})
	if err != nil || profile.Email != olgaEmail || profile.EmailVerified || profile.EmailRequired || profile.DisplayName != "Olga" {
		t.Fatalf("set email: %+v %v", profile, err)
	}
	if olga, err = manager.ValidateAccess(ctx, refreshed.Access); err != nil || olga.EmailRequired {
		t.Fatalf("session still email_required: %+v %v", olga, err)
	}
	mustLogin("", "olga@example.com", []byte("olga password 12"))
	if _, _, err := login("", "olga", []byte("olga password 12")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("username still accepted after email set: %v", err)
	}

	// Changing one's email needs the current password, re-checks uniqueness,
	// leaves it unverified, and signs out other sessions only.
	bobOther, _ := mustLogin("", "bob.builder+ci@example.com", []byte("bob password 123"))
	newBob := "bob@example.org"
	if _, _, err := manager.UpdateProfile(ctx, bob, ProfileChange{Email: &newBob}); !errors.Is(err, ErrCurrentPassword) {
		t.Fatalf("email change without a password: %v", err)
	}
	if _, _, err := manager.UpdateProfile(ctx, bob, ProfileChange{Email: &olgaEmail, CurrentPassword: []byte("bob password 123")}); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("email change to a used email: %v", err)
	}
	profile, revoked, err := manager.UpdateProfile(ctx, bob, ProfileChange{Email: &newBob, CurrentPassword: []byte("bob password 123")})
	if err != nil || revoked != 1 || profile.Email != newBob || profile.EmailVerified {
		t.Fatalf("email change: %+v %d %v", profile, revoked, err)
	}
	if !live(bobTokens) || live(bobOther) {
		t.Fatal("email change did not keep this session and end the other")
	}
	if _, _, err := login("", "bob.builder+ci@example.com", []byte("bob password 123")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("old email still signs in: %v", err)
	}

	// Password change: current password, setup strength, other sessions end.
	bobOther, _ = mustLogin("", newBob, []byte("bob password 123"))
	if _, err := manager.ChangePassword(ctx, bob, []byte("not bobs password"), []byte("bob password 456")); !errors.Is(err, ErrCurrentPassword) {
		t.Fatalf("wrong current password: %v", err)
	}
	if _, err := manager.ChangePassword(ctx, bob, []byte("bob password 123"), []byte("short")); !errors.Is(err, ErrPassword) {
		t.Fatalf("weak password: %v", err)
	}
	if revoked, err := manager.ChangePassword(ctx, bob, []byte("bob password 123"), []byte("bob password 456")); err != nil || revoked != 1 {
		t.Fatalf("change password: %d %v", revoked, err)
	}
	if !live(bobTokens) || live(bobOther) {
		t.Fatal("password change did not keep this session and end the other")
	}
	if _, _, err := login("", newBob, []byte("bob password 123")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("old password still signs in: %v", err)
	}
	limited := false
	for range 12 {
		if _, err := manager.ChangePassword(ctx, bob, []byte("guess password 1"), []byte("bob password 789")); errors.Is(err, ErrRateLimited) {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("current-password checks are not rate-limited")
	}

	// Sessions: only one's own are listed or revocable.
	aliceOther, _ := mustLogin("", "alice@example.com", password)
	sessions, err := manager.ListSessions(ctx, alice)
	if err != nil || len(sessions) != 2 {
		t.Fatalf("list sessions: %+v %v", sessions, err)
	}
	current := 0
	for _, s := range sessions {
		if s.Current {
			current++
			if s.ID != alice.SessionID || s.UserAgent != "Test Browser/1.0" || s.SourceAddress == "" {
				t.Fatalf("current session: %+v", s)
			}
		}
	}
	if current != 1 {
		t.Fatalf("current sessions: %d", current)
	}
	if err := manager.RevokeSession(ctx, bob, aliceOther.SessionID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("revoked another user's session: %v", err)
	}
	if !live(aliceOther) {
		t.Fatal("another user's session was revoked")
	}
	if err := manager.RevokeSession(ctx, alice, alice.SessionID); !errors.Is(err, ErrCurrentSession) {
		t.Fatalf("revoked the current session: %v", err)
	}
	if err := manager.RevokeSession(ctx, alice, aliceOther.SessionID); err != nil || live(aliceOther) {
		t.Fatalf("revoke own session: %v", err)
	}
	mustLogin("", "alice@example.com", password)
	mustLogin("", "alice@example.com", password)
	if n, err := manager.RevokeOtherSessions(ctx, alice); err != nil || n != 2 || !live(aliceTokens) {
		t.Fatalf("revoke other sessions: %d %v", n, err)
	}

	// Admin email repair: owners/admins only, owner rule, audited, revokes.
	if err := users.SetEmail(ctx, bob, olgaID, "o@example.com"); !errors.Is(err, ErrUserAdminDenied) {
		t.Fatalf("member set an email: %v", err)
	}
	if err := users.SetEmail(ctx, alice, olgaID, "alice@example.com"); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("admin set a used email: %v", err)
	}
	if err := users.SetEmail(ctx, alice, olgaID, "bad"); !errors.Is(err, ErrEmailInvalid) {
		t.Fatalf("admin set an invalid email: %v", err)
	}
	olgaTokens, _ = mustLogin("", "olga@example.com", []byte("olga password 12"))
	if err := users.SetEmail(ctx, alice, olgaID, "Olga.New@Example.com"); err != nil || live(olgaTokens) {
		t.Fatalf("admin set email: %v", err)
	}
	mustLogin("", "olga.new@example.com", []byte("olga password 12"))
	if err := users.SetRole(ctx, alice, bob.PrincipalID, "admin"); err != nil {
		t.Fatal(err)
	}
	_, bobAdmin := mustLogin("", newBob, []byte("bob password 456"))
	if err := users.SetEmail(ctx, bobAdmin, owner.PrincipalID, "boss@example.com"); !errors.Is(err, ErrOwnerOnly) {
		t.Fatalf("admin changed an owner's email: %v", err)
	}
	members, err := users.ListMembers(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	emails := map[string]string{}
	for _, m := range members {
		emails[m.Username] = m.Email
	}
	if emails["olga"] != "olga.new@example.com" || emails["alice"] != "alice@example.com" {
		t.Fatalf("member emails: %v", emails)
	}
	if page, total, err := users.ListMembersPage(ctx, alice, MemberFilter{Search: "OLGA.NEW@"}); err != nil || total != 1 || page[0].PrincipalID != olgaID {
		t.Fatalf("search by email: %+v %d %v", page, total, err)
	}

	// Operator repair by handle or email.
	if _, err := OperatorSetEmail(ctx, pool, "olga", "olga@example.net"); err != nil {
		t.Fatal(err)
	}
	if _, err := OperatorSetEmail(ctx, pool, "nobody", "x@example.net"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("operator repair of an unknown account: %v", err)
	}
	var operatorAudits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_audit_events WHERE action='identity.user.email_changed'
		AND actor_kind='operator' AND subject_id=$1 AND detail->>'to'='olga@example.net'`, olgaID).Scan(&operatorAudits); err != nil || operatorAudits != 1 {
		t.Fatalf("operator audit: %d %v", operatorAudits, err)
	}

	// Multiple organizations: the organization is asked for only after the
	// password is right.
	var secondOrg string
	if err := pool.QueryRow(ctx, `INSERT INTO identity_organizations (id,slug,name) VALUES (gen_random_uuid(),'research','Research') RETURNING id`).
		Scan(&secondOrg); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity_memberships (organization_id,principal_id,role) VALUES ($1,$2,'viewer')`, secondOrg, owner.PrincipalID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := login("", "alice@example.com", []byte("wrong password here")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("multi-org wrong password: %v", err)
	}
	if _, _, err := login("", "alice@example.com", password); !errors.Is(err, ErrOrganizationRequired) {
		t.Fatalf("multi-org without slug: %v", err)
	}
	if _, research := mustLogin("research", "alice@example.com", password); research.Role != "viewer" {
		t.Fatalf("multi-org login: %+v", research)
	}

	for action, want := range map[string]int{
		"identity.login_legacy_username":          1,
		"identity.account.email_changed":          2,
		"identity.account.profile_updated":        1,
		"identity.account.password_changed":       1,
		"identity.account.session_revoked":        1,
		"identity.account.other_sessions_revoked": 1,
		"identity.user.email_changed":             2,
	} {
		if got := actions(action); got != want {
			t.Fatalf("audit %s: %d, want %d", action, got, want)
		}
	}
}

func TestNormalizeEmail(t *testing.T) {
	for raw, want := range map[string]string{" A.B+c@Example.COM ": "a.b+c@example.com", "x@y.io": "x@y.io"} {
		if got, err := NormalizeEmail(raw); err != nil || got != want {
			t.Fatalf("%q: %q %v", raw, got, err)
		}
	}
	for _, raw := range []string{"", "a", "a@b", "@b.io", "a@.io", "a@b.io.", "a@b..io", "a b@c.io", "<a@b.io>", "a@b@c.io",
		"\"a\"@b.io", "a@[1.2.3.4]"} {
		if _, err := NormalizeEmail(raw); !errors.Is(err, ErrEmailInvalid) {
			t.Fatalf("%q accepted", raw)
		}
	}
	for email, want := range map[string]string{"bob+ci@x.io": "bob-ci", "1st@x.io": "st0", "_@x.io": "user", "ab@x.io": "ab0"} {
		if got := handleBase(email); got != want {
			t.Fatalf("handle %q: %q want %q", email, got, want)
		}
	}
}
