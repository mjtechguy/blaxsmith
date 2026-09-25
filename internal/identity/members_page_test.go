package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func TestMembersPagePostgres(t *testing.T) {
	pool := identityTestPool(t)
	ctx := tenant.System(context.Background())
	password := []byte("correct horse battery staple")
	if _, err := BootstrapOwner(ctx, pool, "alice@example.com", "engineering", "Engineering", password); err != nil {
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
	tokens, err := manager.LoginLocal(ctx, "engineering", "alice@example.com", password, netip.MustParseAddr("192.0.2.10"), "")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := manager.ValidateAccess(ctx, tokens.Access)
	if err != nil {
		t.Fatal(err)
	}
	// bob: active admin; carol: invited member; dave: disabled viewer; erin: invited viewer.
	ids := map[string]string{}
	for _, m := range [][3]string{{"bob", "Bob Builder", "admin"}, {"carol", "Carol Jones", "member"},
		{"dave", "Dave Smith", "viewer"}, {"erin", "Erin Builder", "viewer"}} {
		link, err := users.Invite(ctx, owner, m[0]+"@example.com", m[1], m[2])
		if err != nil {
			t.Fatal(err)
		}
		ids[m[0]] = link.PrincipalID
		if m[0] == "bob" || m[0] == "dave" {
			if _, err := users.CompleteLink(ctx, link.Token, []byte(m[0]+" password 123"), "", ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := users.SetEnabled(ctx, owner, ids["dave"], false); err != nil {
		t.Fatal(err)
	}
	// Another organization's members never appear.
	if _, err := pool.Exec(ctx, `WITH o AS (INSERT INTO identity_organizations (id,slug,name) VALUES (gen_random_uuid(),'other','Other')
		RETURNING id), p AS (INSERT INTO identity_principals (id,username) VALUES (gen_random_uuid(),'zed') RETURNING id)
		INSERT INTO identity_memberships (organization_id,principal_id,role) SELECT o.id,p.id,'member' FROM o,p`); err != nil {
		t.Fatal(err)
	}

	for _, role := range []string{"member", "viewer"} {
		denied := owner
		denied.Role = role
		if _, _, err := users.ListMembersPage(ctx, denied, MemberFilter{}); !errors.Is(err, ErrUserAdminDenied) {
			t.Fatalf("%s listed members: %v", role, err)
		}
	}
	all, err := users.ListMembers(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	page, total, err := users.ListMembersPage(ctx, owner, MemberFilter{})
	if err != nil || total != 5 || fmt.Sprintf("%+v", page) != fmt.Sprintf("%+v", all) {
		t.Fatalf("default page differs from ListMembers: %+v %d %v, want %+v", page, total, err, all)
	}
	names := func(members []Member) string {
		out := []string{}
		for _, m := range members {
			out = append(out, m.Username)
		}
		return strings.Join(out, " ")
	}
	for _, tc := range []struct {
		f     MemberFilter
		want  string
		total int32
	}{
		{MemberFilter{SortDirection: "desc"}, "erin dave carol bob alice", 5},
		{MemberFilter{SortBy: "username"}, "alice bob carol dave erin", 5},
		{MemberFilter{SortBy: "username", SortDirection: "desc", PageSize: 2, Page: 2}, "carol bob", 5},
		{MemberFilter{SortBy: "created_at", PageSize: 2}, "alice bob", 5},
		{MemberFilter{SortBy: "last_login", SortDirection: "desc", PageSize: 1}, "alice", 5},
		{MemberFilter{Search: "BUILDER"}, "bob erin", 2},
		{MemberFilter{Search: "car"}, "carol", 1},
		{MemberFilter{Roles: []string{"viewer", "admin"}}, "bob dave erin", 3},
		{MemberFilter{Statuses: []string{"invited"}}, "carol erin", 2},
		{MemberFilter{Statuses: []string{"disabled"}}, "dave", 1},
		{MemberFilter{Statuses: []string{"active"}, Roles: []string{"owner", "admin"}}, "alice bob", 2},
		{MemberFilter{Page: 3, PageSize: 3}, "", 5},
	} {
		members, total, err := users.ListMembersPage(ctx, owner, tc.f)
		if err != nil || names(members) != tc.want || total != tc.total {
			t.Fatalf("members %+v: %q %d %v, want %q %d", tc.f, names(members), total, err, tc.want, tc.total)
		}
	}
	if members, _, _ := users.ListMembersPage(ctx, owner, MemberFilter{Statuses: []string{"disabled"}}); members[0].ActiveSessions != 0 {
		t.Fatalf("disabled member sessions: %+v", members)
	}
	if members, _, _ := users.ListMembersPage(ctx, owner, MemberFilter{Search: "alice"}); members[0].ActiveSessions != 1 ||
		members[0].LastLogin == nil || members[0].Status != "active" {
		t.Fatalf("owner row: %+v", members)
	}
	for _, f := range []MemberFilter{{Page: -1}, {PageSize: 101}, {PageSize: -1}, {Page: 10002, PageSize: 1},
		{SortBy: "password"}, {SortDirection: "up"}, {Roles: []string{"guest"}}, {Statuses: []string{"pending"}},
		{Search: strings.Repeat("x", 121)}} {
		if _, _, err := users.ListMembersPage(ctx, owner, f); !errors.Is(err, ErrUserInvalid) {
			t.Fatalf("members %+v accepted: %v", f, err)
		}
	}
}
