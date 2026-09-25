import { useSyncExternalStore } from "react";
import { RefreshCw } from "lucide-react";
import { connectionState, probeNow, subscribeConnection } from "./connectivity";

export const useConnectionState = () => useSyncExternalStore(subscribeConnection, connectionState, () => "online" as const);

// Non-blocking: the page stays usable (and cached data visible) while the
// server restarts; requests retry on their own and resume when it answers.
export function ConnectionBanner() {
  const state = useConnectionState();
  if (state === "online") return null;
  return <div className="connection-banner" role="status" aria-live="polite">
    <RefreshCw size={14} className="spin" aria-hidden="true" />
    <span>Reconnecting… Your session is safe; work resumes when the server is back.</span>
    <button type="button" className="text-action" onClick={() => void probeNow()}>Retry now</button>
  </div>;
}
