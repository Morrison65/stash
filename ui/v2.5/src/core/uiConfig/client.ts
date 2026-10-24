// Page side of the browser-local UI configuration: service worker
// registration, cross-tab cache updates and the troubleshooting actions.

import { ApolloCache, ApolloClient } from "@apollo/client";
import * as GQL from "src/core/generated-graphql";
import { baseURL, getPlatformURL } from "src/core/createClient";
import {
  CHANNEL_NAME,
  IUIConfigUpdatedMessage,
  MESSAGE_UPDATED,
  MESSAGE_WHOAMI,
} from "./engine";
import { UIConfigMap, UI_SYNC_HEADER, isPlainObject } from "./operations";
import { getStoredUIConfig, putUIConfig } from "./store";
import {
  fetchServerUI,
  isIndexedDBAvailable,
  isServiceWorkerControlling,
  tabId,
} from "./link";

export const SERVICE_WORKER_FILE = "sw.js";

// service worker client id of this tab, learned via a whoami message
let swClientId: string | undefined;
let swVersion: string | undefined;

export type UIConfigMode = "service-worker" | "in-page" | "unavailable";

export interface IUIConfigStatus {
  mode: UIConfigMode;
  updatedAt?: number;
  serviceWorkerVersion?: string;
}

function appScope() {
  return new URL(baseURL, window.location.origin);
}

function askWhoAmI() {
  navigator.serviceWorker?.controller?.postMessage({ type: MESSAGE_WHOAMI });
}

/**
 * Registers the UI config service worker with the app's scope (respecting
 * `<base href>`). Only in production builds: the dev server talks to the
 * GraphQL server cross-origin, where the in-page fallback is used.
 */
export function registerServiceWorker() {
  if (!import.meta.env.PROD) return;
  if (!("serviceWorker" in navigator)) return;

  const scope = appScope();
  navigator.serviceWorker
    .register(new URL(SERVICE_WORKER_FILE, scope).href, {
      scope: scope.pathname,
    })
    .catch((e) => {
      // e.g. insecure context or a CSP without worker-src 'self'
      // eslint-disable-next-line no-console
      console.warn(
        "UI config service worker unavailable, using in-page fallback",
        e
      );
    });
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
 * other tabs (or by the service worker on behalf of other tabs).
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
      if (msg.sourceClientId && msg.sourceClientId === swClientId) return;
      writeUIToCache(client.cache, msg.ui);
    };
  }

  if (navigator.serviceWorker) {
    navigator.serviceWorker.addEventListener("message", (e: MessageEvent) => {
      const { data } = e;
      if (isPlainObject(data) && data.type === MESSAGE_WHOAMI) {
        swClientId =
          typeof data.clientId === "string" ? data.clientId : undefined;
        swVersion = typeof data.version === "string" ? data.version : undefined;
      }
    });
    navigator.serviceWorker.addEventListener("controllerchange", askWhoAmI);
    askWhoAmI();
  }
}

export async function getUIConfigStatus(): Promise<IUIConfigStatus> {
  let mode: UIConfigMode = "unavailable";
  if (isServiceWorkerControlling()) mode = "service-worker";
  else if (isIndexedDBAvailable()) mode = "in-page";

  let updatedAt: number | undefined;
  if (isIndexedDBAvailable()) {
    updatedAt = (await getStoredUIConfig())?.updatedAt;
  }
  return { mode, updatedAt, serviceWorkerVersion: swVersion };
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
