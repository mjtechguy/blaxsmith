package tooladapter

// bx is the guest interaction shim (docs/interactive-sessions.md,
// "Structured interactions"). State is plain files under $STATE/ix; every
// write is a hard link of a complete temp file, so readers never see partial
// records, sequence numbers never repeat, and no daemon or lock is needed.
//
//	ix/log/<seq>.json      watch stream: {"seq":N,"interaction"|"event"|"cancelled":…}
//	ix/asked/<id>          interaction was raised (answer target must exist)
//	ix/answers/<id>.json   first answer wins; {"cancelled":true} cancels
//	ix/inbox/<seq>.json    pending steering, consumed by `bx inbox`
//	ix/steered/<id>        steer ids already queued (redelivery is a no-op)

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mjtechguy/blaxsmith/internal/evidence"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	bxOK        = 0
	bxFailed    = 1
	bxInvalid   = 2 // usage, invalid JSON, or unknown interaction
	bxCancelled = 3
)

var (
	bxID   = regexp.MustCompile(`^[0-9A-Za-z_-]{1,64}$`)
	bxPoll = 250 * time.Millisecond
)

func ixPath(parts ...string) string {
	return filepath.Join(append([]string{StateDir(), "ix"}, parts...)...)
}

// Bx runs one bx subcommand and returns its exit code.
func Bx(args []string, stdout, stderr io.Writer) int {
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(stderr, "bx: "+format+"\n", a...)
		return code
	}
	if len(args) == 0 {
		return fail(bxInvalid, "usage: bx ask|event|inbox|watch|answer|steer")
	}
	jsonArg := func(rest []string) (map[string]json.RawMessage, []byte, error) {
		if len(rest) != 2 || rest[0] != "--json" || len(rest[1]) > 64<<10 {
			return nil, nil, errors.New("expected --json '<object>'")
		}
		var object map[string]json.RawMessage
		var compact bytes.Buffer
		if err := json.Unmarshal([]byte(rest[1]), &object); err != nil || object == nil || json.Compact(&compact, []byte(rest[1])) != nil {
			return nil, nil, errors.New("expected one JSON object")
		}
		return object, compact.Bytes(), nil
	}
	str := func(object map[string]json.RawMessage, key string) string {
		var s string
		_ = json.Unmarshal(object[key], &s)
		return s
	}
	switch args[0] {
	case "artifact", "artifacts", "read-artifact", "gate", "gate-receipt", "gate-evidence":
		if err := bxEvidence(args, stdout); err != nil {
			code := bxFailed
			if errors.Is(err, evidence.ErrInvalid) {
				code = bxInvalid
			}
			if errors.Is(err, errEvidenceStopped) {
				code = bxCancelled
			}
			return fail(code, "%v", err)
		}
		return bxOK
	case "ask":
		object, body, err := jsonArg(args[1:])
		id := str(object, "id")
		if err != nil || !bxID.MatchString(id) || !oneOf(str(object, "kind"), "question", "approval", "escalation", "interview_round") {
			return fail(bxInvalid, "ask needs an Interaction with a valid id and kind")
		}
		if created, err := createMarker("asked", id); err != nil {
			return fail(bxFailed, "%v", err)
		} else if created {
			if err := appendRecord("log", "interaction", body); err != nil {
				return fail(bxFailed, "%v", err)
			}
		}
		// Rerunning the same ask after a shell timeout resumes waiting.
		for {
			data, err := os.ReadFile(ixPath("answers", id+".json"))
			if err == nil {
				if isCancel(data) {
					return fail(bxCancelled, "interaction %s was cancelled; stop the current step", id)
				}
				fmt.Fprintf(stdout, "%s\n", data)
				return bxOK
			}
			time.Sleep(bxPoll)
		}
	case "event":
		object, body, err := jsonArg(args[1:])
		kind := str(object, "type")
		if err != nil || !oneOf(kind, "phase", "cycle", "handoff", "finding", "progress", "verdict") ||
			kind == "verdict" && !oneOf(str(object, "status")+str(object, "verdict"), "pass", "fail") {
			return fail(bxInvalid, "event needs a known type (verdict needs status pass|fail)")
		}
		if err := appendRecord("log", "event", body); err != nil {
			return fail(bxFailed, "%v", err)
		}
		return bxOK
	case "steer":
		object, body, err := jsonArg(args[1:])
		id := str(object, "id")
		if err != nil || !bxID.MatchString(id) || !oneOf(str(object, "kind"), "instruction", "pause", "halt", "set_max_cycles") {
			return fail(bxInvalid, "steer needs a valid id and kind")
		}
		created, err := createMarker("steered", id)
		if err != nil {
			return fail(bxFailed, "%v", err)
		}
		if created {
			if err := appendRecord("inbox", "", body); err != nil {
				return fail(bxFailed, "%v", err)
			}
		}
		return bxOK
	case "inbox":
		for _, entry := range sequenced("inbox") {
			data, err := os.ReadFile(entry.path)
			if err == nil && os.Remove(entry.path) == nil { // remove is the claim
				fmt.Fprintf(stdout, "%s\n", data)
			}
		}
		return bxOK
	case "answer":
		if len(args) < 3 || !bxID.MatchString(args[1]) {
			return fail(bxInvalid, "usage: bx answer <id> --json '<Answer>' | --cancel")
		}
		id := args[1]
		if _, err := os.Stat(ixPath("asked", id)); err != nil {
			return fail(bxInvalid, "unknown interaction %s", id)
		}
		body := []byte(`{"cancelled":true}`)
		if args[2] != "--cancel" || len(args) != 3 {
			object, compact, err := jsonArg(args[2:])
			if err != nil || object["interaction_id"] != nil && str(object, "interaction_id") != id {
				return fail(bxInvalid, "answer needs an Answer object for %s", id)
			}
			body = compact
		}
		target := ixPath("answers", id+".json")
		if err := linkNew(target, body); errors.Is(err, fs.ErrExist) {
			// First answer wins. Redelivery is expected, so a later answer is
			// dropped but still exits 0.
			if existing, _ := os.ReadFile(target); !bytes.Equal(existing, body) {
				fmt.Fprintf(stderr, "bx: interaction %s was already answered; kept the first answer\n", id)
			}
			return bxOK
		} else if err != nil {
			return fail(bxFailed, "%v", err)
		}
		if isCancel(body) {
			quoted, _ := json.Marshal(id)
			if err := appendRecord("log", "cancelled", quoted); err != nil {
				return fail(bxFailed, "%v", err)
			}
		}
		return bxOK
	case "watch":
		var cursor int64
		follow := true
		for rest := args[1:]; len(rest) > 0; rest = rest[1:] {
			switch {
			case rest[0] == "--once":
				follow = false
			case rest[0] == "--cursor" && len(rest) > 1:
				n, err := strconv.ParseInt(rest[1], 10, 64)
				if err != nil || n < 0 {
					return fail(bxInvalid, "cursor must be a non-negative integer")
				}
				cursor, rest = n, rest[1:]
			default:
				return fail(bxInvalid, "usage: bx watch [--cursor N] [--once]")
			}
		}
		for {
			for _, entry := range sequenced("log") {
				if entry.seq <= cursor {
					continue
				}
				data, err := os.ReadFile(entry.path)
				if err != nil {
					return fail(bxFailed, "%v", err)
				}
				if _, err := fmt.Fprintf(stdout, "%s\n", data); err != nil {
					return bxFailed
				}
				cursor = entry.seq
			}
			if !follow {
				return bxOK
			}
			time.Sleep(bxPoll)
		}
	}
	return fail(bxInvalid, "unknown subcommand %q", args[0])
}

func oneOf(value string, allowed ...string) bool {
	for _, a := range allowed {
		if value == a {
			return true
		}
	}
	return false
}

func isCancel(data []byte) bool {
	var v struct{ Cancelled bool }
	return json.Unmarshal(data, &v) == nil && v.Cancelled
}

func createMarker(dir, id string) (bool, error) {
	err := linkNew(ixPath(dir, id), nil)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	return err == nil, err
}

// linkNew publishes a complete file at target, failing with fs.ErrExist if
// something is already there.
func linkNew(target string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(ixPath("tmp"), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(ixPath("tmp"), "bx-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Link(tmp.Name(), target)
}

type seqEntry struct {
	seq  int64
	path string
}

func sequenced(dir string) []seqEntry {
	entries, _ := os.ReadDir(ixPath(dir))
	out := make([]seqEntry, 0, len(entries))
	for _, entry := range entries {
		if n, err := strconv.ParseInt(strings.TrimSuffix(entry.Name(), ".json"), 10, 64); err == nil {
			out = append(out, seqEntry{n, ixPath(dir, entry.Name())})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out
}

// appendRecord takes the next free sequence number in dir. Concurrent writers
// race on the link; the loser retries with the next number.
func appendRecord(dir, key string, payload []byte) error {
	var seq int64 = 1
	if entries := sequenced(dir); len(entries) > 0 {
		seq = entries[len(entries)-1].seq + 1
	}
	for ; ; seq++ {
		data := payload
		if key != "" {
			data = fmt.Appendf(nil, `{"seq":%d,%q:%s}`, seq, key, payload)
		}
		err := linkNew(ixPath(dir, fmt.Sprintf("%020d.json", seq)), data)
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
}

func lastVerdict() string {
	verdict := ""
	for _, entry := range sequenced("log") {
		var record struct {
			Event struct{ Type, Status, Verdict string } `json:"event"`
		}
		if data, err := os.ReadFile(entry.path); err == nil && json.Unmarshal(data, &record) == nil && record.Event.Type == "verdict" {
			verdict = record.Event.Status + record.Event.Verdict // validated: exactly one is pass|fail
		}
	}
	return verdict
}

// bxInstructions is appended to every autonomous prompt so each harness routes
// human questions and loop control through the platform instead of its own UI.
const bxInstructions = `

---
Blaxsmith interaction protocol (applies to every step):
- Never ask the human a question in plain output. For any question, approval, choice, or escalation run:
  bx ask --json '{"id":"<unique id [A-Za-z0-9_-]>","kind":"question|approval|escalation|interview_round","title":"...","body_md":"...","options":[{"id":"a","label":"...","description":"...","recommended":true}],"multi_select":false,"allow_free_text":true,"blocking":true}'
  It blocks until answered and prints the Answer JSON. Exit code 3 means cancelled: stop the current step. If your shell tool times out, rerun the identical command; it resumes waiting without asking twice.
- At each phase or loop boundary run ` + "`bx inbox`" + ` and follow every steering line it prints (instruction, pause, halt, set_max_cycles).
- Publish review evidence with bx artifact --json '{"id":"report-1","path":"reports/result.md","kind":"report","title":"Result","renderer":"markdown"}'. Files must be regular workspace files of at most 4 MiB; keep them unchanged until the stage ends.
- Report extension checks with bx gate --json '{"id":"check-1","check":"<declared check>","verdict":"pass|fail|blocked","summary":"...","evidence":[{"path":"reports/result.md","sha256":"<sha256>"}]}'. Commit gate evidence first. Wait for the platform receipt; a reported pass does not replace independent verification.
- Report progress with bx event --json '{"type":"phase|cycle|handoff|finding|progress", ...}'.
- A review or verify stage ends with bx event --json '{"type":"verdict","status":"pass|fail"}'; on fail, your final message lists the findings.
`
