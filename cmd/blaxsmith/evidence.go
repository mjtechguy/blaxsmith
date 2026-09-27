package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/evidence"
	"github.com/mjtechguy/blaxsmith/internal/gitfetch"
	"github.com/mjtechguy/blaxsmith/internal/interact"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// guestOutput never buffers unbounded agent output. The child context closes
// the guest stream when a read fails or exceeds the limit.
func guestOutput(ctx context.Context, g interact.GuestExec, a workflow.Attempt, argv []string, max int) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	p, err := g.Start(ctx, a, argv, false)
	if err != nil {
		return nil, -1, err
	}
	data, err := io.ReadAll(io.LimitReader(p.Stdout, int64(max)+1))
	if err != nil || len(data) > max {
		cancel()
		if err == nil {
			err = workflow.ErrInvalid
		}
		return nil, -1, err
	}
	code, err := p.Wait()
	return data, code, err
}

func gateWatcher(store *workflow.Store, g interact.GuestExec) func(context.Context, workflow.Attempt, []byte) (bool, error) {
	return func(ctx context.Context, a workflow.Attempt, line []byte) (bool, error) {
		var record struct {
			Gate *evidence.Gate `json:"gate"`
		}
		if json.Unmarshal(line, &record) != nil || record.Gate == nil {
			return false, nil
		}
		gate := *record.Gate
		if gate.Validate() != nil {
			return true, nil
		}
		// Exact replays return the durable receipt even after the guest's HEAD moved.
		receipt, found, err := store.GateReceipt(ctx, a, gate)
		if err != nil {
			return true, err
		}
		if !found {
			body, _ := json.Marshal(gate)
			data, code, err := guestOutput(ctx, g, a, []string{"bx", "gate-evidence", "--json", string(body)}, 4096)
			if err != nil {
				return true, err
			}
			var result struct {
				Revision string `json:"revision"`
			}
			reason := ""
			if code != 0 || json.Unmarshal(data, &result) != nil || !gitfetch.IsCommit(result.Revision) {
				reason = "Evidence is missing from the stage commit or its digest does not match."
				result.Revision, err = store.LoadInputCommit(ctx, a)
				if err != nil {
					return true, err
				}
			}
			receipt, err = store.RecordGate(ctx, a, result.Revision, gate, reason)
			if err != nil {
				return true, err
			}
		}
		body, _ := json.Marshal(receipt)
		_, code, err := guestOutput(ctx, g, a, []string{"bx", "gate-receipt", "--json", string(body)}, 4096)
		if err == nil && code != 0 {
			err = errors.New("gate receipt delivery failed")
		}
		return true, err
	}
}

func collectArtifacts(ctx context.Context, store *workflow.Store, g interact.GuestExec, a workflow.Attempt, revision string) error {
	data, code, err := guestOutput(ctx, g, a, []string{"bx", "artifacts"}, 128<<10)
	if err != nil {
		return err
	}
	if code != 0 {
		return workflow.ErrInvalid
	}
	var list []evidence.Artifact
	if json.Unmarshal(data, &list) != nil || len(list) > evidence.MaxArtifacts {
		return workflow.ErrInvalid
	}
	for _, artifact := range list {
		if artifact.Validate() != nil {
			return workflow.ErrInvalid
		}
		data, code, err := guestOutput(ctx, g, a, []string{"bx", "read-artifact", artifact.Path}, evidence.MaxArtifact)
		if err != nil {
			return err
		}
		if code != 0 {
			return workflow.ErrInvalid
		}
		if err = store.RecordArtifact(ctx, a, revision, artifact, data); err != nil {
			return err
		}
	}
	return nil
}
