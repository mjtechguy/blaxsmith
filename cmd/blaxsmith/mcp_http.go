package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Sessionless HTTP binds every request to its own bearer credential. There is
// no retained MCP authority/session that can cross credentials or replicas.
func mcpHTTPHandler(machine http.Handler, resource, challenge string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		values := r.Header.Values("Authorization")
		if r.Header.Get("Cookie") != "" || r.Header.Get("Origin") != "" {
			http.Error(w, "use a machine credential without browser cookies or Origin", http.StatusForbidden)
			return
		}
		if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
			w.Header().Set("WWW-Authenticate", challenge)
			http.Error(w, "machine credential required", http.StatusUnauthorized)
			return
		}
		authorization := values[0]
		invoke := func(ctx context.Context, method string, body []byte) ([]byte, error) {
			ctx = context.WithValue(ctx, machineResourceKey{}, resource)
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://blaxsmith.internal/blaxsmith.api.v1.WorkflowService/"+method, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			request.Header.Set("Authorization", authorization)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Connect-Protocol-Version", "1")
			request.RemoteAddr = r.RemoteAddr
			response := &mcpAPIResponse{headers: make(http.Header)}
			machine.ServeHTTP(response, request)
			if response.overflow {
				return nil, errors.New("Blaxsmith response too large")
			}
			data := response.body.Bytes()
			if response.status != http.StatusOK {
				var detail struct{ Code, Message string }
				if json.Unmarshal(data, &detail) == nil && detail.Code != "" {
					return nil, fmt.Errorf("%s: %s", detail.Code, detail.Message)
				}
				return nil, errors.New("Blaxsmith API unavailable; reconcile durable state before retrying")
			}
			return data, nil
		}
		server, err := mcpToolServer(r.Context(), invoke)
		if err != nil {
			w.Header().Set("WWW-Authenticate", challenge)
			http.Error(w, "machine credential unavailable or expired", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}).ServeHTTP(w, r)
	})
}

// A bounded in-process HTTP response keeps the real Connect handler and its
// authorization/audit interceptors on the path, without loopback TLS or secrets
// in URLs. WorkflowService methods are unary; streaming is not exposed here.
type mcpAPIResponse struct {
	headers  http.Header
	body     bytes.Buffer
	status   int
	overflow bool
}

func (w *mcpAPIResponse) Header() http.Header { return w.headers }
func (w *mcpAPIResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *mcpAPIResponse) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if len(data) > 4<<20-w.body.Len() {
		w.overflow = true
		return 0, io.ErrShortBuffer
	}
	return w.body.Write(data)
}
