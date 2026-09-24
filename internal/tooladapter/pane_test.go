package tooladapter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/recipe"
)

var haveTmux bool

// The test binary doubles as the pane process so Run can be exercised under
// real tmux without building the worker separately.
func TestMain(m *testing.M) {
	if path, err := exec.LookPath("tmux"); err == nil {
		tmuxBinary, haveTmux = path, true
	}
	if len(os.Args) > 1 && os.Args[1] == "pane" {
		os.Exit(Pane(os.Args[2:]))
	}
	// Unix socket paths are short (104 bytes on macOS), so avoid $TMPDIR.
	state, err := os.MkdirTemp("/tmp", "bxt")
	if err != nil {
		panic(err)
	}
	os.Setenv("BLAXSMITH_STATE_DIR", state)
	paneBinary, _ = os.Executable()
	code := m.Run()
	os.RemoveAll(state)
	os.Exit(code)
}

func requireTmux(t *testing.T) {
	t.Helper()
	if !haveTmux {
		t.Skip("tmux is not installed; the harness runs inside a tmux pane")
	}
}

// Lines recorded from the pinned CLIs against a loopback mock provider
// (codex-cli 0.156.1, Claude Code 2.1.281, opencode v2.0.14). Tool-call lines
// follow each CLI's documented event shape.
func TestRenderEventPerHarness(t *testing.T) {
	tests := []struct {
		name, line, session, final, text string
	}{
		{"codex thread", `{"type":"thread.started","thread_id":"01a0d3c4-bf67-7783-8262-265379800e97"}`, "01a0d3c4-bf67-7783-8262-265379800e97", "", "codex session 01a0d3c4"},
		{"codex message", `{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"PROBE_OK"}}`, "", "PROBE_OK", "PROBE_OK"},
		{"codex command", `{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"bash -lc ls","aggregated_output":"","exit_code":null,"status":"in_progress"}}`, "", "", "$ bash -lc ls"},
		{"codex failed command", `{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"false","exit_code":1,"status":"failed"}}`, "", "", "exit 1"},
		{"codex turn failed", `{"type":"turn.failed","error":{"message":"stream disconnected"}}`, "", "", "stream disconnected"},
		{"codex usage", `{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1}}`, "", "", ""},
		{"claude init", `{"type":"system","subtype":"init","cwd":"/workspace/source","session_id":"26e5c96b-1bc0-4e30-924b-dd75918ce667","tools":["Bash","Edit","Read"],"mcp_servers":[],"model":"claude-opus-5-5","permissionMode":"default"}`, "26e5c96b-1bc0-4e30-924b-dd75918ce667", "", "claude session 26e5c96b"},
		{"claude text", `{"type":"assistant","message":{"id":"msg_probe","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"PROBE_OK"}]},"parent_tool_use_id":null,"session_id":"26e5c96b-1bc0-4e30-924b-dd75918ce667"}`, "26e5c96b-1bc0-4e30-924b-dd75918ce667", "PROBE_OK", "PROBE_OK"},
		{"claude tool", `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"go test ./..."}}]},"session_id":"s"}`, "s", "", `→ Bash {"command":"go test ./..."}`},
		{"claude tool error", `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":"boom"}]},"session_id":"s"}`, "s", "", "tool error"},
		{"claude result", `{"type":"result","subtype":"success","is_error":false,"duration_ms":20,"result":"PROBE_OK","stop_reason":"end_turn","session_id":"26e5c96b-1bc0-4e30-924b-dd75918ce667"}`, "26e5c96b-1bc0-4e30-924b-dd75918ce667", "PROBE_OK", "result: success"},
		{"opencode step", `{"type":"step_start","timestamp":1790259614812,"sessionID":"ses_f2c362c34ffeY779hJxisXyO1g","part":{"id":"prt_0d3c9d45c001QEeFjCe57XSC1P","sessionID":"ses_f2c362c34ffeY779hJxisXyO1g","messageID":"msg_0d3c9d44e001MT5I1w8x2Jl1ip","type":"step-start"}}`, "ses_f2c362c34ffeY779hJxisXyO1g", "", ""},
		{"opencode text", `{"type":"text","timestamp":1790259614815,"sessionID":"ses_f2c362c34ffeY779hJxisXyO1g","part":{"id":"prt_0d3c9d45e001fZ4DJY0yinMrEW","type":"text","text":"PROBE_OK","time":{"start":1790259614814,"end":1790259614815}}}`, "ses_f2c362c34ffeY779hJxisXyO1g", "PROBE_OK", "PROBE_OK"},
		{"opencode tool", `{"type":"tool_use","sessionID":"ses_1","part":{"type":"tool","tool":"bash","state":{"status":"completed","input":{"command":"ls"}}}}`, "ses_1", "", `→ bash {"command":"ls"}`},
		{"opencode error", `{"type":"error","sessionID":"ses_1","error":{"name":"APIError","data":{"message":"rate limited"}}}`, "ses_1", "", "rate limited"},
		{"plain stderr-ish text", `not json`, "", "", "not json"},
	}
	for _, tc := range tests {
		text, session, final := renderEvent([]byte(tc.line + "\n"))
		if session != tc.session || final != tc.final || !strings.Contains(text, tc.text) || (tc.text == "" && text != "") {
			t.Errorf("%s: got text=%q session=%q final=%q", tc.name, text, session, final)
		}
	}
}

func fakeHarness(t *testing.T, harness, version, body string) Runtime {
	t.Helper()
	script := []byte("#!/bin/sh\ncase \"$1\" in --version) echo '" + version + "'; exit;; login) exit 0;; esac\n" + body)
	binary := filepath.Join(t.TempDir(), harness)
	if err := os.WriteFile(binary, script, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(script)
	return Runtime{Harness: harness, Image: "example/tool@sha256:" + strings.Repeat("a", 64), Binary: binary,
		BinarySHA256: hex.EncodeToString(sum[:]), Version: "0.156.1", Supported: []ModelEffort{{Model: "gpt-6-luna", Effort: "high"}}}
}

func readState(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(statePath(name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func TestPaneRunsHarnessUnderTmuxAndRecordsSession(t *testing.T) {
	requireTmux(t)
	events := `{"type":"thread.started","thread_id":"0199-thread"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"All done"}}
{"type":"turn.completed","usage":{}}`
	runtime := fakeHarness(t, "codex", "codex-cli 0.156.1", "printf '%s\\n' '"+strings.ReplaceAll(events, "\n", "' '")+"'\necho oops >&2\nexit 3\n")
	in, err := Prepare(runtime, recipe.Profile{Harness: "codex", Model: "gpt-6-luna", Effort: "high"}, "Do the work", time.Minute, 4096)
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	output, err := Run(t.Context(), in, workdir, []string{"OPENAI_API_KEY=leased-secret-value"})
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("exit code must reach the existing error path: %v", err)
	}
	if string(output) != events+"\noops\n" {
		t.Fatalf("events.jsonl and stderr must be read back verbatim: %q", output)
	}
	if readState(t, "session-id") != "0199-thread" || readState(t, "exit") != "3" {
		t.Fatalf("session or exit not recorded")
	}
	var result map[string]string
	if err := json.Unmarshal([]byte(readState(t, "result.json")), &result); err != nil || result["summary"] != "All done" ||
		result["schema"] != "blaxsmith.attempt-result/v1alpha1" || result["revision"] != "" {
		t.Fatalf("result.json: %v %v", result, err)
	}
	argv, err := ResumeArgv()
	want := []string{runtime.Binary, "resume", "--disable", "multi_agent", "--disable", "apps", "--disable", "plugins", "--sandbox", "workspace-write",
		"--model", "gpt-6-luna", "--config", `approval_policy="never"`, "--config", `model_reasoning_effort="high"`, "--config", `web_search="disabled"`,
		"--config", "skills.bundled.enabled=false", "--config", `projects={"` + workdir + `"={trust_level="trusted"}}`, "0199-thread"}
	if err != nil || !reflect.DeepEqual(argv, want) {
		t.Fatalf("resume argv:\n got %q\nwant %q (%v)", argv, want, err)
	}
}

// Mirrors the terminal gateway's takeover and handback sequence exactly.
func TestTakeoverResumesInteractivePaneAndHandsBack(t *testing.T) {
	requireTmux(t)
	body := `if [ "$1" = resume ]; then
  echo "resumed $* in $PWD" > "$BLAXSMITH_STATE_DIR/resumed"
  while read -r line; do [ "$line" = /exit ] && exit 0; done
  exit 1
fi
echo '{"type":"thread.started","thread_id":"t-42"}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"halfway"}}'
exec sleep 30
`
	runtime := fakeHarness(t, "codex", "codex-cli 0.156.1", body)
	in, err := Prepare(runtime, recipe.Profile{Harness: "codex", Model: "gpt-6-luna", Effort: "high"}, "Do the work", time.Minute, 4096)
	if err != nil {
		t.Fatal(err)
	}
	workdir, _ := filepath.EvalSymlinks(t.TempDir())
	_ = os.Remove(statePath("session-id")) // left by an earlier test
	done := make(chan error, 1)
	go func() {
		_, err := Run(t.Context(), in, workdir, []string{"OPENAI_API_KEY=leased-secret-value"})
		done <- err
	}()
	tmux := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(tmuxBinary, append([]string{"-S", statePath("tmux.sock")}, args...)...).Output()
		if err != nil {
			t.Fatalf("tmux %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	eventually := func(what string, ok func() bool) {
		t.Helper()
		for i := 0; i < 150; i++ {
			if ok() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	exists := func(name string) func() bool {
		return func() bool { _, err := os.Stat(statePath(name)); return err == nil }
	}
	eventually("session id", exists("session-id"))
	tmux("send-keys", "-t", "agent", "C-c")
	eventually("pane_dead", func() bool { return tmux("display-message", "-p", "-t", "agent", "#{pane_dead}") == "1" })
	if _, err := os.Stat(statePath("exit")); err == nil || readState(t, "owner") != "human" {
		t.Fatal("an interrupted autonomous pane must not record a finish")
	}
	argv, err := ResumeArgv()
	if err != nil {
		t.Fatal(err)
	}
	tmux(append([]string{"respawn-pane", "-k", "-t", "agent", "--", paneBinary, "pane", "--interactive", "--"}, argv...)...)
	eventually("resumed TUI", exists("resumed"))
	if got := readState(t, "resumed"); !strings.HasPrefix(got, "resumed resume --disable multi_agent") || !strings.HasSuffix(got, " t-42 in "+workdir) {
		t.Fatalf("interactive pane did not resume the native session in the workdir: %q", got)
	}
	select {
	case err := <-done:
		t.Fatalf("worker finished during takeover: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	tmux("send-keys", "-t", "agent", "-l", "/exit")
	time.Sleep(time.Second)
	tmux("send-keys", "-t", "agent", "Enter")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handback must complete the attempt: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("worker did not finish after handback")
	}
	var result map[string]string
	if err := json.Unmarshal([]byte(readState(t, "result.json")), &result); err != nil || !strings.HasPrefix(result["summary"], "halfway\n\n[A human took over") {
		t.Fatalf("takeover summary: %v %v", result, err)
	}
}

func TestAutonomousFinishBeatsLateTakeover(t *testing.T) {
	requireTmux(t)
	runtime := fakeHarness(t, "codex", "codex-cli 0.156.1", `echo '{"type":"thread.started","thread_id":"t-1"}'`+"\n")
	in, err := Prepare(runtime, recipe.Profile{Harness: "codex", Model: "gpt-6-luna", Effort: "high"}, "Do the work", time.Minute, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), in, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	if readState(t, "owner") != "agent" || claimEnd("human") {
		t.Fatal("a finished stage must refuse a late takeover claim")
	}
}

func TestPaneSquashesStageChangesOntoFrozenCommitAndBundles(t *testing.T) {
	requireTmux(t)
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	base := run("rev-parse", "HEAD")
	// The agent commits once itself and leaves another change uncommitted.
	body := `echo agent > a.txt
git -c user.name=agent -c user.email=a@a -c commit.gpgsign=false commit -qam "agent wip"
echo new > b.txt
echo '{"type":"item.completed","item":{"type":"agent_message","text":"implemented"}}'
`
	runtime := fakeHarness(t, "codex", "codex-cli 0.156.1", body)
	in, err := Prepare(runtime, recipe.Profile{Harness: "codex", Model: "gpt-6-luna", Effort: "high"}, "Do the work", time.Minute, 4096)
	if err != nil {
		t.Fatal(err)
	}
	in.base = base
	if _, err := Run(t.Context(), in, repo, nil); err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err := json.Unmarshal([]byte(readState(t, "result.json")), &result); err != nil {
		t.Fatal(err)
	}
	head := run("rev-parse", "HEAD")
	if result["revision"] != head || result["summary"] != "implemented" || head == base || run("rev-parse", "HEAD^") != base ||
		run("log", "-1", "--format=%an") != "blaxsmith-agent" || run("status", "--porcelain") != "" {
		t.Fatalf("stage must be one bot commit on the frozen base: %v head=%s", result, head)
	}
	if heads := run("bundle", "list-heads", statePath("result.bundle")); !strings.Contains(heads, head) {
		t.Fatalf("bundle must carry the stage commit: %q", heads)
	}
}
