// In-page fallback for the UI config service worker: an Apollo link with the
// same behaviour (see src/sw/sw.ts), active only while no service worker
// controls the page - e.g. over plain http on a LAN address, where service
// workers are unavailable (no secure context), or before the first
// service worker activation. Raw `fetch('/graphql')` calls by plugins are
// not covered by this fallback.

import {
  ApolloLink,
  FetchResult,
  Observable,
  Operation,
} from "@apollo/client/core";
import {
  IExecutionResult,
  UI_SYNC_HEADER,
  analyzeOperation,
  overrideMutationResult,
} from "./operations";
import {
  CHANNEL_NAME,
  IUIConfigUpdatedMessage,
  SERVER_UI_QUERY,
  UIConfigEngine,
  serverUIFromResult,
} from "./engine";

/** Identifies this tab in broadcasts sent by the in-page fallback. */
export const tabId = `tab-${Math.random().toString(36).slice(2)}`;

export function isServiceWorkerControlling() {
  return (
    typeof navigator !== "undefined" &&
    !!navigator.serviceWorker &&
    !!navigator.serviceWorker.controller
  );
}

export function isIndexedDBAvailable() {
  try {
    return typeof indexedDB !== "undefined" && !!indexedDB;
  } catch {
    return false;
  }
}

function hasBypassHeader(operation: Operation) {
  const { headers }: { headers?: unknown } = operation.getContext();
  if (!headers || typeof headers !== "object") return false;
  const h = headers as Record<string, unknown>;
  return Object.keys(h).some(
    (k) => k.toLowerCase() === UI_SYNC_HEADER.toLowerCase() && h[k] === "1"
  );
}

/** Fetches the server's UI config, bypassing local handling. */
export async function fetchServerUI(graphqlURL: string) {
  const res = await fetch(graphqlURL, {
    method: "POST",
    credentials: "same-origin",
    headers: {
      "Content-Type": "application/json",
      [UI_SYNC_HEADER]: "1",
    },
    body: JSON.stringify(SERVER_UI_QUERY),
  });
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  return serverUIFromResult(await res.json());
}

export function createUIConfigLink(options: { graphqlURL: string }) {
  const channel =
    typeof BroadcastChannel !== "undefined"
      ? new BroadcastChannel(CHANNEL_NAME)
      : undefined;

  const engine = new UIConfigEngine({
    fetchServerUI: () => fetchServerUI(options.graphqlURL),
    notify: (msg: IUIConfigUpdatedMessage) => channel?.postMessage(msg),
  });

  return new ApolloLink((operation, forward) => {
    if (
      isServiceWorkerControlling() ||
      !isIndexedDBAvailable() ||
      hasBypassHeader(operation)
    ) {
      return forward(operation);
    }

    const plan = analyzeOperation(operation.query, operation.operationName);
    const { variables } = operation;

    if (plan.kind === "passthrough") return forward(operation);

    if (plan.kind === "local") {
      return new Observable<FetchResult>((observer) => {
        let sub: { unsubscribe: () => void } | undefined;
        let closed = false;
        engine
          .localMutationResult(
            operation.query,
            operation.operationName,
            plan.fields,
            variables,
            tabId
          )
          .then(
            (result) => {
              if (closed) return;
              observer.next(result as FetchResult);
              observer.complete();
            },
            () => {
              // storage failed: let the server handle it
              if (closed) return;
              sub = forward(operation).subscribe(observer);
            }
          );
        return () => {
          closed = true;
          sub?.unsubscribe();
        };
      });
    }

    // query or mixed mutation: forward and post-process the result(s)
    return new Observable<FetchResult>((observer) => {
      let sub: { unsubscribe: () => void } | undefined;
      let closed = false;

      const rewrite = async (r: FetchResult): Promise<FetchResult> => {
        const result = r as IExecutionResult;
        if (plan.kind === "query") {
          return (await engine.rewriteQuery(
            result,
            plan.targets
          )) as FetchResult;
        }
        return r;
      };

      const start = async () => {
        let mixed: Awaited<ReturnType<typeof engine.applyMutation>> | undefined;
        if (plan.kind === "mixed") {
          try {
            mixed = await engine.applyMutation(plan.fields, variables, tabId);
          } catch {
            mixed = undefined;
          }
        }
        if (closed) return;

        // keep results ordered even though post-processing is async
        let chain = Promise.resolve();
        sub = forward(operation).subscribe({
          next: (r) => {
            chain = chain.then(async () => {
              let out = r;
              try {
                out = mixed
                  ? (overrideMutationResult(
                      r as IExecutionResult,
                      mixed.results
                    ) as FetchResult)
                  : await rewrite(r);
              } catch {
                out = r;
              }
              if (!closed) observer.next(out);
            });
          },
          error: (e) => {
            chain = chain.then(() => {
              if (!closed) observer.error(e);
            });
          },
          complete: () => {
            chain = chain.then(() => {
              if (!closed) observer.complete();
            });
          },
        });
      };

      start();

      return () => {
        closed = true;
        sub?.unsubscribe();
      };
    });
  });
}
