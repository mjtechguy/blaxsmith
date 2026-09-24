import { useEffect, useRef } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { terminalUrl, type TerminalFrame, type TerminalState } from "./run-control";

export type TerminalLink = { status: "connecting" | "open" | "closed" | "ended"; message?: string };

function tokenTheme() {
  const css = getComputedStyle(document.documentElement);
  const token = (name: string) => css.getPropertyValue(name).trim();
  return { background: token("--terminal-bg"), foreground: token("--terminal-text"), cursor: token("--primary"),
    cursorAccent: token("--terminal-bg"), selectionBackground: token("--primary-soft") };
}

// Speaks the browser <-> Blaxsmith terminal protocol in docs/interactive-sessions.md.
export function AttemptTerminal({ attemptId, inControl, onState, onLink }: {
  attemptId: string; inControl: boolean; onState: (state: TerminalState) => void; onLink: (link: TerminalLink) => void;
}) {
  const host = useRef<HTMLDivElement>(null);
  const term = useRef<Terminal | null>(null);
  const callbacks = useRef({ onState, onLink });
  callbacks.current = { onState, onLink };

  useEffect(() => {
    const terminal = new Terminal({ fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace", fontSize: 13, scrollback: 5000, theme: tokenTheme() });
    const fit = new FitAddon();
    terminal.loadAddon(fit);
    terminal.open(host.current!);
    fit.fit();
    const resize = new ResizeObserver(() => fit.fit());
    resize.observe(host.current!);
    const themeWatch = new MutationObserver(() => { terminal.options.theme = tokenTheme(); });
    themeWatch.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] });
    term.current = terminal;
    return () => { resize.disconnect(); themeWatch.disconnect(); terminal.dispose(); term.current = null; };
  }, []);

  // Reconnects when control changes so the server re-attaches read-only or read-write.
  useEffect(() => {
    const terminal = term.current!;
    const encoder = new TextEncoder();
    let socket: WebSocket | undefined;
    let timer: number | undefined;
    let delay = 1_000;
    let stopped = false;
    terminal.options.disableStdin = !inControl;
    terminal.options.cursorBlink = inControl;
    const sendSize = () => {
      if (inControl && socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify({ type: "resize", cols: terminal.cols, rows: terminal.rows }));
    };
    const connect = () => {
      callbacks.current.onLink({ status: "connecting" });
      const ws = socket = new WebSocket(terminalUrl(attemptId));
      ws.binaryType = "arraybuffer";
      ws.onopen = () => { delay = 1_000; terminal.reset(); callbacks.current.onLink({ status: "open" }); sendSize(); };
      ws.onmessage = ({ data }) => {
        if (typeof data !== "string") { terminal.write(new Uint8Array(data as ArrayBuffer)); return; }
        let frame: TerminalFrame;
        try { frame = JSON.parse(data); } catch { return; }
        if (frame.type === "state") callbacks.current.onState(frame);
        else if (frame.type === "exit") { stopped = true; callbacks.current.onLink({ status: "ended", message: `Session exited with code ${frame.code}.` }); }
        // Transient server errors fall through to onclose and retry; access errors are final.
        else if (frame.type === "error" && !/unavailable|disconnected/.test(frame.message)) { stopped = true; callbacks.current.onLink({ status: "ended", message: frame.message }); }
      };
      ws.onclose = () => {
        if (stopped || socket !== ws) return;
        callbacks.current.onLink({ status: "closed" });
        timer = window.setTimeout(connect, delay);
        delay = Math.min(delay * 2, 30_000);
      };
    };
    const input = terminal.onData((data) => { if (inControl && socket?.readyState === WebSocket.OPEN) socket.send(encoder.encode(data)); });
    const resized = terminal.onResize(sendSize);
    connect();
    return () => { stopped = true; window.clearTimeout(timer); input.dispose(); resized.dispose(); socket?.close(); };
  }, [attemptId, inControl]);

  return <div ref={host} className={`terminal-host ${inControl ? "" : "terminal-readonly"}`} aria-label={inControl ? "Interactive agent terminal" : "Read-only agent terminal"} />;
}
