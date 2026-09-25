// Live channels that survive backend restarts without a page reload.
import { currentSession } from "./auth";
import { backoffDelay } from "./connectivity";

export type LiveState = "connecting" | "connected" | "reconnecting";

// A run activity stream. EventSource reconnects by itself after a network drop
// or a stream the server ended (a drain sends "retry: 1000"), resuming from
// Last-Event-ID. It gives up for good on an HTTP error (401 after the access
// cookie expired, or a proxy's 502/503 during a restart); then this renews
// the session through the refresh cookie and reopens with capped, jittered
// backoff from the latest cursor. sessionEnded runs if the refresh is refused.
export function openLiveStream(url: () => string, on: {
  open: () => void; message: (data: string) => void; state: (state: LiveState) => void; sessionEnded: () => void;
}): () => void {
  let source: EventSource | undefined;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let attempt = 0;
  let closed = false;
  const connect = () => {
    if (closed) return;
    const stream = source = new EventSource(url());
    stream.onopen = () => { attempt = 0; on.state("connected"); on.open(); };
    stream.onmessage = ({ data }) => on.message(data);
    stream.onerror = () => {
      if (closed) return;
      on.state("reconnecting");
      if (stream.readyState !== EventSource.CLOSED) return;
      timer = setTimeout(() => void reopen(), backoffDelay(attempt++));
    };
  };
  const reopen = async () => {
    try {
      if (!(await currentSession())) { if (!closed) on.sessionEnded(); return; }
    } catch { /* still unreachable: try the stream anyway, then back off again */ }
    connect();
  };
  on.state("connecting");
  connect();
  return () => { closed = true; clearTimeout(timer); source?.close(); };
}

// The terminal socket's reconnect delay: a 1001 "going away" (a draining
// server) reconnects almost at once; anything else backs off.
export function socketRetryDelay(closeCode: number, attempt: number, random: () => number = Math.random): number {
  return closeCode === 1001 ? 250 + Math.round(random() * 500) : backoffDelay(attempt, random);
}
