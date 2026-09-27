package tooladapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/evidence"
)

// readEvidence refuses symlinks and non-regular files. OpenRoot also prevents
// an ancestor replacement from escaping the checkout during the read.
func readEvidence(dir, name string) ([]byte, error) {
	if !evidence.Path(name) {
		return nil, evidence.ErrInvalid
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	parts := strings.Split(name, "/")
	for i := range parts {
		info, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (i < len(parts)-1 && !info.IsDir()) ||
			(i == len(parts)-1 && (!info.Mode().IsRegular() || info.Size() > evidence.MaxArtifact)) {
			return nil, evidence.ErrInvalid
		}
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, evidence.MaxArtifact+1))
	if err != nil || len(data) > evidence.MaxArtifact {
		return nil, evidence.ErrInvalid
	}
	return data, nil
}

func bxEvidence(args []string, out io.Writer) error {
	switch args[0] {
	case "artifact":
		if len(args) != 3 || args[1] != "--json" {
			return evidence.ErrInvalid
		}
		var a evidence.Artifact
		if strictJSON([]byte(args[2]), &a) != nil {
			return evidence.ErrInvalid
		}
		l, err := readLaunch()
		if err != nil {
			return err
		}
		data, err := readEvidence(l.Dir, a.Path)
		if err != nil {
			return err
		}
		a.SHA256 = evidence.SHA(data)
		if err = a.Validate(); err != nil {
			return err
		}
		entries, err := os.ReadDir(ixPath("artifacts"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		target := ixPath("artifacts", a.ID+".json")
		body, _ := json.Marshal(a)
		if old, err := os.ReadFile(target); err == nil {
			if !bytes.Equal(old, body) {
				return evidence.ErrInvalid
			}
		} else {
			if len(entries) >= evidence.MaxArtifacts {
				return evidence.ErrInvalid
			}
			if err = linkNew(target, body); err != nil {
				return err
			}
		}
		return json.NewEncoder(out).Encode(map[string]string{"artifact_id": a.ID, "sha256": a.SHA256})
	case "artifacts":
		if len(args) != 1 {
			return evidence.ErrInvalid
		}
		entries, err := os.ReadDir(ixPath("artifacts"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if len(entries) > evidence.MaxArtifacts {
			return evidence.ErrInvalid
		}
		list := []evidence.Artifact{}
		for _, e := range entries {
			data, err := os.ReadFile(ixPath("artifacts", e.Name()))
			if err != nil || len(data) > 4096 {
				return evidence.ErrInvalid
			}
			var a evidence.Artifact
			if strictJSON(data, &a) != nil || a.Validate() != nil {
				return evidence.ErrInvalid
			}
			list = append(list, a)
		}
		return json.NewEncoder(out).Encode(list)
	case "read-artifact":
		if len(args) != 2 {
			return evidence.ErrInvalid
		}
		l, err := readLaunch()
		if err != nil {
			return err
		}
		data, err := readEvidence(l.Dir, args[1])
		if err != nil {
			return err
		}
		_, err = out.Write(data)
		return err
	case "gate":
		if len(args) != 3 || args[1] != "--json" || len(args[2]) > 64<<10 {
			return evidence.ErrInvalid
		}
		var g evidence.Gate
		if strictJSON([]byte(args[2]), &g) != nil || g.Validate() != nil {
			return evidence.ErrInvalid
		}
		body, _ := json.Marshal(g)
		target := ixPath("gates", g.ID+".json")
		if err := linkNew(target, body); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		old, err := os.ReadFile(target)
		if err != nil || !bytes.Equal(old, body) {
			return evidence.ErrInvalid
		}
		// Appending again repairs a crash between registration and log publication;
		// the platform deduplicates by id and rejects changed replays.
		if err := appendRecord("log", "gate", body); err != nil {
			return err
		}
		for {
			data, err := os.ReadFile(ixPath("gate-receipts", g.ID+".json"))
			if err == nil {
				var receipt evidence.Receipt
				if strictJSON(data, &receipt) != nil || receipt.GateID != g.ID {
					return evidence.ErrInvalid
				}
				if _, err = fmt.Fprintf(out, "%s\n", data); err != nil {
					return err
				}
				if !receipt.Accepted {
					return errors.New("gate refused")
				}
				return nil
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if _, err := os.Stat(statePath("stopped")); err == nil {
				return errEvidenceStopped
			}
			time.Sleep(bxPoll)
		}
	case "gate-evidence":
		if len(args) != 3 || args[1] != "--json" {
			return evidence.ErrInvalid
		}
		var g evidence.Gate
		if strictJSON([]byte(args[2]), &g) != nil || g.Validate() != nil {
			return evidence.ErrInvalid
		}
		l, err := readLaunch()
		if err != nil {
			return err
		}
		head, err := exec.Command("git", "-C", l.Dir, "rev-parse", "HEAD").Output()
		if err != nil {
			return err
		}
		revision := strings.TrimSpace(string(head))
		for _, ref := range g.Evidence {
			data, err := readEvidence(l.Dir, ref.Path)
			if err != nil || evidence.SHA(data) != ref.SHA256 {
				return evidence.ErrInvalid
			}
			// cat-file streams a bounded blob; no revision/path is interpreted by a shell.
			cmd := exec.Command("git", "-C", l.Dir, "cat-file", "blob", revision+":"+ref.Path)
			pipe, err := cmd.StdoutPipe()
			if err != nil {
				return err
			}
			if err = cmd.Start(); err != nil {
				return err
			}
			committed, readErr := io.ReadAll(io.LimitReader(pipe, evidence.MaxArtifact+1))
			if len(committed) > evidence.MaxArtifact {
				_ = cmd.Process.Kill()
			}
			waitErr := cmd.Wait()
			if readErr != nil || waitErr != nil || evidence.SHA(committed) != ref.SHA256 {
				return evidence.ErrInvalid
			}
		}
		return json.NewEncoder(out).Encode(map[string]string{"revision": revision})
	case "gate-receipt":
		if len(args) != 3 || args[1] != "--json" {
			return evidence.ErrInvalid
		}
		var r evidence.Receipt
		if strictJSON([]byte(args[2]), &r) != nil || !evidence.ID.MatchString(r.GateID) || len(r.Reason) > 1000 {
			return evidence.ErrInvalid
		}
		body, _ := json.Marshal(r)
		err := linkNew(ixPath("gate-receipts", r.GateID+".json"), body)
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	}
	return evidence.ErrInvalid
}

var errEvidenceStopped = errors.New("attempt stopped")
