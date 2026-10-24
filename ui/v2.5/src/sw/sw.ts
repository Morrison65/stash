// Stash service worker: keeps the free-form UI configuration
// (`configuration.ui`) in IndexedDB instead of on the server.
//
// It intercepts JSON `POST <scope>graphql` requests of every page and every
// plugin (Apollo or raw fetch):
// - queries selecting `configuration { ui }` are forwarded, then the `ui`
//   values are replaced with the IndexedDB value (seeded from the server on
//   first use);
// - mutations made only of `configureUI` / `configureUISetting` are answered
//   locally and never reach the server;
// - mutations mixing those with other fields are forwarded unchanged and the
//   UI part is applied locally as well.
// Requests with the header `X-Stash-UI-Sync: 1` are not touched. Any
// internal error falls back to forwarding the original request.
//
// Built as a classic single-file script (build/sw.js) by the swPlugin in
// vite.config.js. See docs/architecture/ui-config.md.

import { DocumentNode, parse } from "graphql";
import {
  IExecutionResult,
  OperationPlan,
  UI_SYNC_HEADER,
  analyzeOperation,
  isPlainObject,
  overrideMutationResult,
} from "../core/uiConfig/operations";
import {
  CHANNEL_NAME,
  IUIConfigUpdatedMessage,
  MESSAGE_WHOAMI,
  SERVER_UI_QUERY,
  UIConfigEngine,
  serverUIFromResult,
} from "../core/uiConfig/engine";

// Bump when the request handling changes in a way worth noting in logs.
const SW_VERSION = "1";

// Minimal typings for the service worker global scope: the app's tsconfig
// uses the DOM lib, which conflicts with the WebWorker lib.
interface IExtendableEvent extends Event {
  waitUntil(promise: Promise<unknown>): void;
}

interface IFetchEvent extends IExtendableEvent {
  readonly request: Request;
  readonly clientId: string;
  respondWith(response: Response | Promise<Response>): void;
}

interface IExtendableMessageEvent extends IExtendableEvent {
  readonly data: unknown;
  readonly source: { id: string; postMessage(message: unknown): void } | null;
}

interface IServiceWorkerScope {
  readonly registration: { readonly scope: string };
  readonly clients: { claim(): Promise<void> };
  skipWaiting(): Promise<void>;
  addEventListener(
    type: "install" | "activate",
    listener: (event: IExtendableEvent) => void
  ): void;
  addEventListener(type: "fetch", listener: (event: IFetchEvent) => void): void;
  addEventListener(
    type: "message",
    listener: (event: IExtendableMessageEvent) => void
  ): void;
}

const sw = globalThis as unknown as IServiceWorkerScope;

const LOG_PREFIX = `[stash-sw v${SW_VERSION}]`;

function logError(message: string, e: unknown) {
  // eslint-disable-next-line no-console
  console.warn(`${LOG_PREFIX} ${message}`, e);
}

function graphqlURL() {
  return new URL("graphql", sw.registration.scope).href;
}

const channel =
  typeof BroadcastChannel !== "undefined"
    ? new BroadcastChannel(CHANNEL_NAME)
    : undefined;

async function fetchServerUI() {
  const res = await fetch(graphqlURL(), {
    method: "POST",
    credentials: "same-origin",
    headers: {
      "Content-Type": "application/json",
      [UI_SYNC_HEADER]: "1",
    },
    body: JSON.stringify(SERVER_UI_QUERY),
  });
  if (!res.ok) return undefined;
  return serverUIFromResult(await res.json());
}

const engine = new UIConfigEngine({
  fetchServerUI,
  notify: (msg: IUIConfigUpdatedMessage) => channel?.postMessage(msg),
});

// parsed documents by query text; the app sends the same few queries
const documentCache = new Map<string, DocumentNode>();
const DOCUMENT_CACHE_SIZE = 200;

function parseCached(query: string) {
  let doc = documentCache.get(query);
  if (!doc) {
    doc = parse(query);
    if (documentCache.size >= DOCUMENT_CACHE_SIZE) {
      const oldest = documentCache.keys().next().value;
      if (oldest !== undefined) documentCache.delete(oldest);
    }
    documentCache.set(query, doc);
  }
  return doc;
}

interface IRequestBody {
  query?: unknown;
  operationName?: unknown;
  variables?: unknown;
}

interface IPlannedOperation {
  body: IRequestBody;
  doc?: DocumentNode;
  plan: OperationPlan;
}

function planBody(body: unknown): IPlannedOperation {
  if (!isPlainObject(body) || typeof body.query !== "string") {
    // e.g. persisted queries without a query text
    return { body: {}, plan: { kind: "passthrough" } };
  }
  const doc = parseCached(body.query);
  const name =
    typeof body.operationName === "string" ? body.operationName : undefined;
  return { body, doc, plan: analyzeOperation(doc, name) };
}

function operationName(op: IPlannedOperation) {
  return typeof op.body.operationName === "string"
    ? op.body.operationName
    : undefined;
}

function variables(op: IPlannedOperation) {
  return isPlainObject(op.body.variables) ? op.body.variables : undefined;
}

function isJSONResponse(res: Response) {
  return (
    res.type !== "opaque" &&
    res.type !== "opaqueredirect" &&
    (res.headers.get("Content-Type") ?? "").includes("json")
  );
}

function jsonResponse(data: unknown, base?: Response) {
  const headers = new Headers(base?.headers);
  headers.delete("Content-Length");
  headers.delete("Content-Encoding");
  headers.set("Content-Type", "application/json");
  return new Response(JSON.stringify(data), {
    status: base?.status ?? 200,
    statusText: base?.statusText ?? "OK",
    headers,
  });
}

function forwardRequest(original: Request, body: string) {
  return new Request(original.url, {
    method: "POST",
    headers: original.headers,
    body,
    credentials: original.credentials,
    cache: original.cache,
    redirect: original.redirect,
    referrer: original.referrer,
    referrerPolicy: original.referrerPolicy,
    mode: original.mode === "navigate" ? "same-origin" : original.mode,
  });
}

/** Applies the UI part of a mixed mutation; failures are logged only. */
async function applyMixed(op: IPlannedOperation, clientId: string) {
  if (op.plan.kind !== "mixed") return undefined;
  try {
    return await engine.applyMutation(op.plan.fields, variables(op), clientId);
  } catch (e) {
    logError("could not apply UI config locally", e);
    return undefined;
  }
}

/** Post-processes a network result for a query or mixed mutation. */
async function postProcess(
  op: IPlannedOperation,
  result: IExecutionResult,
  mixed: Awaited<ReturnType<typeof applyMixed>>
) {
  if (op.plan.kind === "query") {
    return engine.rewriteQuery(result, op.plan.targets);
  }
  if (mixed) return overrideMutationResult(result, mixed.results);
  return result;
}

async function handleSingle(
  original: Request,
  op: IPlannedOperation,
  clientId: string
): Promise<Response> {
  const { plan, doc } = op;

  if (plan.kind === "local" && doc) {
    let result: IExecutionResult;
    try {
      result = await engine.localMutationResult(
        doc,
        operationName(op),
        plan.fields,
        variables(op),
        clientId
      );
    } catch (e) {
      logError("local UI config write failed, forwarding to server", e);
      return fetch(original);
    }
    return jsonResponse(result);
  }

  const mixed = await applyMixed(op, clientId);
  const res = await fetch(original);
  try {
    if (!isJSONResponse(res)) return res;
    const result = await res.clone().json();
    if (!isPlainObject(result)) return res;
    return jsonResponse(await postProcess(op, result, mixed), res);
  } catch (e) {
    logError("could not rewrite response", e);
    return res;
  }
}

async function handleBatch(
  original: Request,
  ops: IPlannedOperation[],
  clientId: string
): Promise<Response> {
  const results: (IExecutionResult | undefined)[] = [];
  const forwarded: number[] = [];
  const mixed: Awaited<ReturnType<typeof applyMixed>>[] = [];

  // apply in request order, as the server would
  for (let i = 0; i < ops.length; i += 1) {
    const op = ops[i];
    if (op.plan.kind === "local" && op.doc) {
      // eslint-disable-next-line no-await-in-loop
      results[i] = await engine.localMutationResult(
        op.doc,
        operationName(op),
        op.plan.fields,
        variables(op),
        clientId
      );
    } else {
      // eslint-disable-next-line no-await-in-loop
      mixed[i] = await applyMixed(op, clientId);
      forwarded.push(i);
    }
  }

  if (forwarded.length === 0) return jsonResponse(results);

  const res = await fetch(
    forwardRequest(original, JSON.stringify(forwarded.map((i) => ops[i].body)))
  );
  try {
    if (!isJSONResponse(res)) return res;
    const networkResults = await res.clone().json();
    if (
      !Array.isArray(networkResults) ||
      networkResults.length !== forwarded.length
    ) {
      return res;
    }
    await Promise.all(
      forwarded.map(async (opIndex, j) => {
        const r = networkResults[j];
        results[opIndex] = isPlainObject(r)
          ? await postProcess(ops[opIndex], r, mixed[opIndex])
          : (r as IExecutionResult);
      })
    );
    return jsonResponse(results, res);
  } catch (e) {
    logError("could not rewrite batched response", e);
    return res;
  }
}

async function handleGraphQL(
  request: Request,
  clientId: string
): Promise<Response> {
  const original = request.clone();

  let ops: IPlannedOperation[];
  let batched: boolean;
  try {
    const text = await request.text();
    // cheap pre-check: most operations do not touch the UI config
    if (!text.includes("configuration") && !text.includes("configureUI")) {
      return await fetch(original);
    }
    const json: unknown = JSON.parse(text);
    batched = Array.isArray(json);
    ops = (Array.isArray(json) ? json : [json]).map(planBody);
  } catch (e) {
    // unparsable requests are left for the server to reject
    return fetch(original);
  }

  if (ops.length === 0 || ops.every((o) => o.plan.kind === "passthrough")) {
    return fetch(original);
  }

  // handleSingle/handleBatch fall back internally once the request has been
  // forwarded; errors before that end up here.
  const fallback = original.clone();
  try {
    return batched
      ? await handleBatch(original, ops, clientId)
      : await handleSingle(original, ops[0], clientId);
  } catch (e) {
    logError("UI config handling failed, forwarding request", e);
    return fetch(fallback);
  }
}

sw.addEventListener("install", (event) => {
  event.waitUntil(sw.skipWaiting());
});

sw.addEventListener("activate", (event) => {
  event.waitUntil(sw.clients.claim());
});

sw.addEventListener("message", (event) => {
  const { data, source } = event;
  if (isPlainObject(data) && data.type === MESSAGE_WHOAMI && source) {
    source.postMessage({
      type: MESSAGE_WHOAMI,
      clientId: source.id,
      version: SW_VERSION,
    });
  }
});

sw.addEventListener("fetch", (event) => {
  const { request } = event;
  if (request.method !== "POST") return;

  const url = new URL(request.url);
  url.search = "";
  url.hash = "";
  if (url.href !== graphqlURL()) return;

  if (request.headers.get(UI_SYNC_HEADER) === "1") return;
  // multipart uploads etc. are not inspected
  const contentType = request.headers.get("Content-Type") ?? "";
  if (!contentType.toLowerCase().includes("application/json")) return;

  event.respondWith(handleGraphQL(request, event.clientId));
});
