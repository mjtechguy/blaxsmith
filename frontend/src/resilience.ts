// TanStack Query policy for a backend that restarts: queries and mutations
// retry only while the server is unreachable (capped exponential backoff with
// jitter), everything refetches once it answers again, and a refused refresh
// (auth.ts) clears the session so the root layout routes to sign-in.
import { QueryClient } from "@tanstack/react-query";
import { onSessionExpired, sessionQueryKey } from "./auth";
import { backoffDelay, isUnavailable, onReconnected } from "./connectivity";

export const QUERY_RETRIES = 8; // ~2 minutes of backoff; reconnection refetches after that
export const MUTATION_RETRIES = 3;

export function createAppQueryClient(): QueryClient {
  const queryClient = new QueryClient({ defaultOptions: {
    queries: { staleTime: 300_000, retry: (count, error) => isUnavailable(error) && count < QUERY_RETRIES, retryDelay: (count) => backoffDelay(count) },
    mutations: { retry: (count, error) => isUnavailable(error) && count < MUTATION_RETRIES, retryDelay: (count) => backoffDelay(count) },
  } });
  onSessionExpired(() => queryClient.setQueryData(sessionQueryKey, null));
  onReconnected(() => {
    void queryClient.invalidateQueries();
    void queryClient.resumePausedMutations();
  });
  return queryClient;
}
