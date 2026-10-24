// Page side of the browser-local UI configuration: cross-tab cache updates,
// the troubleshooting actions, and removal of the former service worker.

import { ApolloCache, ApolloClient } from "@apollo/client";
import * as GQL from "src/core/generated-graphql";
import { baseURL, getPlatformURL } from "src/core/createClient";
import {
  CHANNEL_NAME,
  IUIConfigUpdatedMessage,
  MESSAGE_UPDATED,
} from "./engine";
import { UIConfigMap, UI_SYNC_HEADER, isPlainObject } from "./operations";
import { getStoredUIConfig, putUIConfig } from "./store";
import { fetchServerUI, isIndexedDBAvailable, tabId } from "./link";

// The UI config used to be handled by a service worker at <base>sw.js. It
// added latency to every GraphQL request, so the in-page Apollo link
// (link.ts) does it alone now; sw.js is a script that removes itself.
const FORMER_SERVICE_WORKER_FILE = "sw.js";

export type UIConfigMode = "in-page" | "unavailable";

export interface IUIConfigStatus {
  mode: UIConfigMode;
  updatedAt?: number;
}

/** Unregisters the former UI config service worker of this app, if any. */
export function removeServiceWorker() {
  if (!("serviceWorker" in navigator)) return;
  const scope = new URL(baseURL, window.location.origin).href;
  navigator.serviceWorker
    .getRegistrations()
    .then((registrations) =>
      Promise.all(
        registrations
          .filter(
            (r) =>
              r.scope === scope &&
              [r.active, r.waiting, r.installing].some((w) =>
                w?.scriptURL.endsWith(`/${FORMER_SERVICE_WORKER_FILE}`)
              )
          )
          .map((r) => r.unregister())
      )
    )
    .catch(() => {});
}

function writeUIToCache(cache: ApolloCache<unknown>, ui: UIConfigMap) {
  const existing = cache.readQuery<GQL.ConfigurationQuery>({
    query: GQL.ConfigurationDocument,
  });
  if (!existing?.configuration) return;

  cache.writeQuery<GQL.ConfigurationQuery>({
    query: GQL.ConfigurationDocument,
    data: {
      configuration: {
        ...existing.configuration,
        ui,
      },
    },
  });
}

/**
 * Keeps this tab's Apollo cache in sync with UI config changes made in
 * other tabs.
 */
export function startUIConfigSync(client: ApolloClient<unknown>) {
  if (typeof BroadcastChannel !== "undefined") {
    const channel = new BroadcastChannel(CHANNEL_NAME);
    channel.onmessage = (e: MessageEvent) => {
      const msg = e.data as IUIConfigUpdatedMessage | undefined;
      if (!msg || msg.type !== MESSAGE_UPDATED || !isPlainObject(msg.ui)) {
        return;
      }
      // our own writes already updated the cache via the mutation result
      if (msg.sourceClientId && msg.sourceClientId === tabId) return;
      writeUIToCache(client.cache, msg.ui);
    };
  }
}

export async function getUIConfigStatus(): Promise<IUIConfigStatus> {
  if (!isIndexedDBAvailable()) return { mode: "unavailable" };
  const updatedAt = (await getStoredUIConfig())?.updatedAt;
  return { mode: "in-page", updatedAt };
}

function graphqlURL() {
  return getPlatformURL("graphql").toString();
}

/**
 * Sends the IndexedDB UI config to the server (`configureUI(input)`) with
 * the bypass header, so that it really reaches the server.
 */
export async function syncUIConfigToServer() {
  const stored = await getStoredUIConfig();
  if (!stored) throw new Error("No UI configuration stored in this browser");

  const res = await fetch(graphqlURL(), {
    method: "POST",
    credentials: "same-origin",
    headers: {
      "Content-Type": "application/json",
      [UI_SYNC_HEADER]: "1",
    },
    body: JSON.stringify({
      operationName: "StashUIConfigSync",
      query:
        "mutation StashUIConfigSync($input: Map) { configureUI(input: $input) }",
      variables: { input: stored.ui },
    }),
  });
  if (!res.ok) throw new Error(`HTTP ${res.status} ${res.statusText}`);
  const json: unknown = await res.json();
  if (isPlainObject(json) && Array.isArray(json.errors) && json.errors.length) {
    const first: unknown = json.errors[0];
    throw new Error(
      isPlainObject(first) && typeof first.message === "string"
        ? first.message
        : "GraphQL error"
    );
  }
}

/**
 * Overwrites the IndexedDB UI config with the server's value and notifies
 * other tabs. The caller should reload the page afterwards.
 */
export async function loadUIConfigFromServer() {
  const ui = await fetchServerUI(graphqlURL());
  if (!ui) throw new Error("Server returned no UI configuration");
  const updatedAt = await putUIConfig(ui);
  if (typeof BroadcastChannel !== "undefined") {
    const channel = new BroadcastChannel(CHANNEL_NAME);
    const msg: IUIConfigUpdatedMessage = {
      type: MESSAGE_UPDATED,
      ui,
      updatedAt,
      sourceClientId: tabId,
    };
    channel.postMessage(msg);
    channel.close();
  }
}
