package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1/apiv1connect"
	"github.com/mjtechguy/blaxsmith/internal/extension"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func TestExtensionErrorCodes(t *testing.T) {
	for err, want := range map[error]connect.Code{
		workflow.ErrExtensionDenied: connect.CodePermissionDenied,
		extension.ErrApproval:       connect.CodeInvalidArgument,
		workflow.ErrConflict:        connect.CodeAlreadyExists,
		workflow.ErrNotFound:        connect.CodeNotFound,
		&workflow.ExtensionInvalidError{Field: &extension.Error{Path: "id", Message: "bad"}}: connect.CodeInvalidArgument,
	} {
		if got := connect.CodeOf(extensionError(err)); got != want {
			t.Fatalf("%v mapped to %v, want %v", err, got, want)
		}
	}
}

// testExtensionBrowserAPI checks the ExtensionService through the real HTTPS
// handler. Every role reads; only owners/admins preview, install, or check.
// Nothing here reaches the network: owner requests use sources that fail
// validation before any fetch.
func testExtensionBrowserAPI(t *testing.T, ctx context.Context, client *http.Client, origin, csrf string, admin bool) {
	t.Helper()
	c := apiv1connect.NewExtensionServiceClient(client, origin+"/api")
	list := connect.NewRequest(&api.ListExtensionsRequest{})
	list.Header().Set("Origin", origin)
	if _, err := c.ListExtensions(ctx, list); err != nil {
		t.Fatalf("list extensions: %v", err)
	}
	source := &api.ExtensionSource{RepositoryUrl: "https://example.com/not/allowed", GitRef: "main"}
	preview := connect.NewRequest(&api.PreviewExtensionInstallRequest{Source: source})
	preview.Header().Set("Origin", origin)
	preview.Header().Set("X-Blaxsmith-CSRF", csrf)
	install := connect.NewRequest(&api.InstallExtensionRequest{Source: source, ExpectedCommit: strings.Repeat("a", 40)})
	install.Header().Set("Origin", origin)
	install.Header().Set("X-Blaxsmith-CSRF", csrf)
	check := connect.NewRequest(&api.CheckExtensionUpdateRequest{ExtensionId: "00000000-0000-0000-0000-000000000001"})
	check.Header().Set("Origin", origin)
	check.Header().Set("X-Blaxsmith-CSRF", csrf)
	_, previewErr := c.PreviewExtensionInstall(ctx, preview)
	_, installErr := c.InstallExtension(ctx, install)
	_, checkErr := c.CheckExtensionUpdate(ctx, check)
	if !admin {
		for name, err := range map[string]error{"preview": previewErr, "install": installErr, "check": checkErr} {
			if connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Fatalf("non-admin %s: %v", name, err)
			}
		}
		return
	}
	if connect.CodeOf(previewErr) != connect.CodeInvalidArgument || connect.CodeOf(installErr) != connect.CodeInvalidArgument ||
		connect.CodeOf(checkErr) != connect.CodeNotFound {
		t.Fatalf("owner requests: %v, %v, %v", previewErr, installErr, checkErr)
	}
	noCSRF := connect.NewRequest(&api.PreviewExtensionInstallRequest{Source: source})
	noCSRF.Header().Set("Origin", origin)
	if _, err := c.PreviewExtensionInstall(ctx, noCSRF); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("preview without CSRF: %v", err)
	}
}
