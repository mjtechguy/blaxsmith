package tooladapter

// Guest-side terminal session: the harness runs inside a tmux pane so the
// platform can attach a live terminal and a human can take over. See
// docs/interactive-sessions.md for the shared contract.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/mjtechguy/blaxsmith/internal/extension"
)

var (
	tmuxBinary = "/usr/bin/tmux"
	paneBinary = WorkerBinary
	revision   = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
)

// StateDir holds the tmux socket and per-attempt guest files. Tests override
// it through the environment; the worker passes it into the tool environment.
func StateDir() string {
	if dir := os.Getenv("BLAXSMITH_STATE_DIR"); dir != "" {
		return dir
	}
	return "/tmp/blaxsmith"
}

func statePath(name string) string { return filepath.Join(StateDir(), name) }

// launch is the frozen launch configuration the worker writes before starting
// tmux. Pane and resume-argv read it; neither needs the public task JSON.
type launch struct {
	Harness string   `json:"harness"`
	Dir     string   `json:"dir"`
	Base    string   `json:"base,omitempty"` // frozen commit the stage started from
	Tmux    string   `json:"tmux"`
	Run     []string `json:"run"`
	Resume  []string `json:"resume"` // session id is appended
	// Signals map an embedded extension's MCP tool calls to bx events.
	Signals []extension.Signal `json:"signals,omitempty"`
}

func readLaunch() (launch, error) {
	var l launch
	data, err := os.ReadFile(statePath("launch.json"))
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal(data, &l); err != nil || len(l.Run) == 0 || len(l.Resume) == 0 {
		return l, fmt.Errorf("%w: invalid launch configuration", ErrBlocked)
	}
	return l, nil
}

// ResumeArgv returns the harness-native resume argv for the recorded session.
func ResumeArgv() ([]string, error) {
	l, err := readLaunch()
	if err != nil {
		return nil, err
	}
	id, err := os.ReadFile(statePath("session-id"))
	session := strings.TrimSpace(string(id))
	if err != nil || session == "" || strings.ContainsAny(session, " \t\r\n\x00") || strings.HasPrefix(session, "-") {
		return nil, fmt.Errorf("%w: no native session id recorded", ErrBlocked)
	}
	return append(append([]string(nil), l.Resume...), session), nil
}

func writeAtomic(name string, data []byte, mode os.FileMode) error {
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}

// claimEnd lets exactly one of "the autonomous harness finished" and "a human
// took over" win. Only the winner of the autonomous race signals done.
func claimEnd(owner string) bool {
	f, err := os.OpenFile(statePath("owner"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return false
	}
	_, _ = f.WriteString(owner)
	return f.Close() == nil
}

// Pane is `blaxsmith-tool-worker pane [--interactive] [-- argv...]`. Without
// argv it runs the frozen launch argv (autonomous) or the resume argv
// (interactive), so user text never passes through tmux's command parser.
func Pane(args []string) int {
	interactive := len(args) > 0 && args[0] == "--interactive"
	if interactive {
		args = args[1:]
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	l, launchErr := readLaunch()
	if len(args) == 0 {
		switch {
		case launchErr != nil:
			fmt.Fprintln(os.Stderr, launchErr)
			return 2
		case interactive:
			resume, err := ResumeArgv()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
			args = resume
		default:
			args = l.Run
		}
	}
	if interactive && launchErr == nil {
		prepareInteractive(l)
	}
	// Takeover sends Ctrl-C to the pane's foreground process group: the harness
	// exits and this process learns the exit was an interrupt, not a finish.
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt)
	cmd := exec.Command(args[0], args[1:]...) // #nosec G204 -- argv comes from the worker's verified launch
	var summary, session string
	var streamed chan struct{}
	if interactive {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	} else {
		events, err := os.OpenFile(statePath("events.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer events.Close()
		stderrLog, err := os.OpenFile(statePath("stderr.log"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer stderrLog.Close()
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		cmd.Stderr = io.MultiWriter(stderrLog, redactor{os.Stderr})
		streamed = make(chan struct{})
		work := paneActivity(l.Dir)
		go func() {
			defer close(streamed)
			defer work.Close() // held work-log records are written before done is signalled
			reader := bufio.NewReaderSize(stdout, 64<<10)
			for {
				line, err := reader.ReadBytes('\n')
				if len(line) > 0 {
					_, _ = events.Write(line)
					text, id, final := renderEvent(line)
					work.Observe(bytes.TrimSpace(line))
					for _, event := range signalEvents(bytes.TrimSpace(line), l.Signals) {
						_ = appendRecord("log", "event", event)
					}
					if id != "" && session == "" {
						session = id
						_ = writeAtomic(statePath("session-id"), []byte(id+"\n"), 0600)
					}
					if final != "" {
						summary = final
					}
					if text != "" {
						_, _ = redactor{os.Stdout}.Write([]byte(text + "\n"))
					}
				}
				if err != nil {
					return
				}
			}
		}()
	}
	code := 0
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 127
	} else {
		if streamed != nil {
			<-streamed
		}
		code = exitCode(cmd.Wait())
	}
	if interactive {
		summary = interactiveSummary()
	} else if len(interrupted) > 0 || !claimEnd("agent") {
		// Interrupted for takeover: never signal done. With remain-on-exit the
		// dead pane waits for respawn-pane to start the resumed TUI.
		claimEnd("human")
		fmt.Println("\n[blaxsmith] harness stopped for human takeover")
		return 130
	}
	if err := writeResult(summary, l.Base); err != nil {
		fmt.Fprintln(os.Stderr, "[blaxsmith] result:", err)
		if code == 0 {
			code = 1
		}
	}
	_ = writeAtomic(statePath("exit"), []byte(strconv.Itoa(code)+"\n"), 0600)
	if err := exec.Command(l.Tmux, "-S", statePath("tmux.sock"), "wait-for", "-S", "blaxsmith-done").Run(); err != nil { // #nosec G204 -- worker-recorded tmux, fixed argv
		fmt.Fprintln(os.Stderr, "signal done:", err)
	}
	fmt.Printf("\n[blaxsmith] harness exited with code %d\n", code)
	return code
}

func exitCode(err error) int {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exit.ExitCode()
	default:
		return 1
	}
}

// redactor hides the leased provider key or Codex tokens from anything
// rendered to viewers.
type redactor struct{ w io.Writer }

func (r redactor) Write(p []byte) (int, error) {
	if _, err := r.w.Write(redactSecrets(p, leasedSecrets())); err != nil {
		return 0, err
	}
	return len(p), nil
}

// prepareInteractive does the harness-specific first-run setup a native TUI
// needs and headless mode does not. It runs inside the pane, where the leased
// key is already in the environment, so the key reaches disk only on takeover.
func prepareInteractive(l launch) {
	home := os.Getenv("HOME")
	switch l.Harness {
	case "codex":
		// The Codex TUI ignores the key env until an API-key login exists.
		// A native key is CODEX_API_KEY; a gateway token is OPENAI_API_KEY.
		// A delivered ChatGPT sign-in is already $CODEX_HOME/auth.json, and
		// an API-key login would overwrite it.
		key := os.Getenv("CODEX_API_KEY")
		if key == "" {
			key = os.Getenv("OPENAI_API_KEY")
		}
		if key == "" {
			return
		}
		login := exec.Command(l.Run[0], "login", "--with-api-key") // #nosec G204 -- verified binary
		login.Stdin = strings.NewReader(key)
		_ = login.Run()
	case "claude-code":
		// Skip onboarding, the custom-key confirmation, and the trust dialog.
		name := filepath.Join(home, ".claude.json")
		state := map[string]any{}
		if data, err := os.ReadFile(name); err == nil {
			_ = json.Unmarshal(data, &state)
		}
		key := os.Getenv("ANTHROPIC_API_KEY")
		if len(key) > 20 {
			key = key[len(key)-20:]
		}
		projects, _ := state["projects"].(map[string]any)
		if projects == nil {
			projects = map[string]any{}
		}
		project, _ := projects[l.Dir].(map[string]any)
		if project == nil {
			project = map[string]any{}
		}
		project["hasTrustDialogAccepted"] = true
		projects[l.Dir] = project
		state["projects"] = projects
		state["hasCompletedOnboarding"] = true
		state["theme"] = "dark"
		state["customApiKeyResponses"] = map[string]any{"approved": []string{key}, "rejected": []string{}}
		if data, err := json.Marshal(state); err == nil {
			_ = writeAtomic(name, data, 0600)
		}
	}
}

// renderEvent turns one harness JSON line into a concise terminal line, the
// native session id when present, and the final assistant text when present.
func renderEvent(line []byte) (text, session, final string) {
	var e struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		ThreadID  string `json:"thread_id"`
		SessionID string `json:"session_id"`
		OpenCode  string `json:"sessionID"`
		Model     string `json:"model"`
		Result    string `json:"result"`
		IsError   bool   `json:"is_error"`
		Message   json.RawMessage
		Error     json.RawMessage
		Item      struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Command  string `json:"command"`
			ExitCode *int   `json:"exit_code"`
			Message  string `json:"message"`
			Changes  []struct {
				Path, Kind string
			} `json:"changes"`
		} `json:"item"`
		Part struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Tool  string          `json:"tool"`
			State json.RawMessage `json:"state"`
		} `json:"part"`
	}
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return "", "", ""
	}
	if json.Unmarshal(trimmed, &e) != nil {
		return clip(string(trimmed)), "", ""
	}
	session = firstNonEmpty(e.ThreadID, e.SessionID, e.OpenCode)
	switch e.Type {
	// Codex `exec --json`
	case "thread.started":
		text = "● codex session " + e.ThreadID
	case "item.started":
		if e.Item.Type == "command_execution" {
			text = "$ " + clip(e.Item.Command)
		}
	case "item.completed":
		switch e.Item.Type {
		case "agent_message":
			text, final = e.Item.Text, e.Item.Text
		case "command_execution":
			if e.Item.ExitCode != nil && *e.Item.ExitCode != 0 {
				text = fmt.Sprintf("  ✗ exit %d", *e.Item.ExitCode)
			}
		case "file_change":
			var paths []string
			for _, c := range e.Item.Changes {
				paths = append(paths, c.Kind+" "+c.Path)
			}
			text = "✎ " + clip(strings.Join(paths, ", "))
		case "error":
			text = "✗ " + e.Item.Message
		}
	case "turn.failed", "error":
		text = "✗ " + clip(errorText(e.Error, e.Message))
	// Claude Code `--output-format stream-json`
	case "system":
		if e.Subtype == "init" {
			text = "● claude session " + e.SessionID + " (" + e.Model + ")"
		}
	case "assistant":
		var m struct {
			Content []struct {
				Type, Text, Name string
				Input            json.RawMessage
			}
		}
		_ = json.Unmarshal(e.Message, &m)
		var lines []string
		for _, c := range m.Content {
			switch c.Type {
			case "text":
				lines = append(lines, c.Text)
				final = c.Text
			case "tool_use":
				lines = append(lines, "→ "+c.Name+" "+clip(string(c.Input)))
			}
		}
		text = strings.Join(lines, "\n")
	case "user":
		var m struct {
			Content []struct {
				Type    string `json:"type"`
				IsError bool   `json:"is_error"`
			}
		}
		_ = json.Unmarshal(e.Message, &m)
		for _, c := range m.Content {
			if c.Type == "tool_result" && c.IsError {
				text = "  ✗ tool error"
			}
		}
	case "result":
		final = e.Result
		text = "● result: " + e.Subtype
		if e.IsError {
			text = "✗ result: " + e.Subtype + " " + clip(e.Result)
		}
	// OpenCode `run --format json`
	case "text":
		text, final = e.Part.Text, e.Part.Text
	case "tool_use":
		var s struct {
			Status string          `json:"status"`
			Input  json.RawMessage `json:"input"`
		}
		_ = json.Unmarshal(e.Part.State, &s)
		text = "→ " + e.Part.Tool + " " + clip(string(s.Input))
		if s.Status == "error" {
			text += " ✗"
		}
	}
	return text, session, final
}

func errorText(values ...json.RawMessage) string {
	for _, raw := range values {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return s
		}
		var m struct {
			Message string `json:"message"`
			Data    struct {
				Message string `json:"message"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &m) == nil && m.Message+m.Data.Message != "" {
			return m.Message + m.Data.Message
		}
	}
	return "error"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func clip(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 240 {
		return strings.ToValidUTF8(s[:240], "") + "…"
	}
	return s
}

func lastSummary() string {
	data, _ := os.ReadFile(statePath("events.jsonl"))
	summary := ""
	for _, line := range bytes.Split(data, []byte("\n")) {
		if _, _, final := renderEvent(line); final != "" {
			summary = final
		}
	}
	return summary
}

func interactiveSummary() string {
	note := "[A human took over this stage in the native terminal; that session's transcript is not included in this summary.]"
	if s := lastSummary(); s != "" {
		return s + "\n\n" + note
	}
	return note
}

const maxBundle = 64 << 20

// writeResult records the bounded handoff for downstream stages: the last
// assistant message, the resulting revision, and the verdict reported through
// `bx event`. The result is written even when committing fails.
func writeResult(summary, base string) error {
	const max = 16 << 10
	if len(summary) > max {
		summary = strings.ToValidUTF8(summary[:max], "")
	}
	head, commitErr := commitStage(base)
	data, err := json.Marshal(map[string]string{
		"schema":   "blaxsmith.attempt-result/v1alpha1",
		"summary":  summary,
		"revision": head,
		"verdict":  lastVerdict(),
	})
	if err == nil {
		err = writeAtomic(statePath("result.json"), data, 0644)
	}
	return errors.Join(commitErr, err)
}

// commitStage squashes the stage's work (agent or human) into one bot commit
// on top of the frozen base and bundles base..HEAD so the platform can push
// it to the run branch.
func commitStage(base string) (string, error) {
	bundle := statePath("result.bundle")
	_ = os.Remove(bundle)
	env := append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false",
			"-c", "user.name=blaxsmith-agent", "-c", "user.email=blaxsmith-agent@users.noreply.invalid"}, args...)...)
		cmd.Env = env
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	if _, err := git("rev-parse", "--show-toplevel"); err != nil {
		return "", nil // not a repository: nothing to hand off
	}
	if _, err := git("add", "-A"); err != nil {
		return "", fmt.Errorf("stage changes: %w", err)
	}
	if base != "" {
		if _, err := git("reset", "--soft", base); err != nil {
			return "", fmt.Errorf("squash onto frozen commit: %w", err)
		}
	}
	if _, err := git("diff", "--cached", "--quiet"); err != nil {
		if _, err := git("commit", "-q", "--no-verify", "-m", "blaxsmith: stage changes"); err != nil {
			return "", fmt.Errorf("commit stage changes: %w", err)
		}
	}
	head, err := git("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || !revision.MatchString(head) {
		return "", fmt.Errorf("read resulting revision: %v", err)
	}
	if base != "" && head != base {
		if _, err := git("bundle", "create", bundle, base+"..HEAD"); err != nil {
			return head, fmt.Errorf("bundle stage commit: %w", err)
		}
		if info, err := os.Stat(bundle); err != nil || info.Size() > maxBundle {
			_ = os.Remove(bundle)
			return head, fmt.Errorf("stage commit bundle is missing or larger than %d bytes", maxBundle)
		}
	}
	return head, nil
}
