// Exercises the service worker's fetch handling end to end, with a mocked
// network and fake-indexeddb standing in for the browser.
import "fake-indexeddb/auto";
import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest";

const SCOPE = "https://stash.test/prefix/";
const GRAPHQL = `${SCOPE}graphql`;

type Listener = (event: unknown) => void;
const listeners: Record<string, Listener> = {};
const broadcasts: unknown[] = [];

interface IFakeFetchEvent {
  request: Request;
  clientId: string;
  response?: Promise<Response>;
  respondWith(r: Promise<Response>): void;
}

// what the fake server answers, and what it received
let serverUI: Record<string, unknown> = {};
const serverRequests: { headers: Headers; body: unknown }[] = [];

function serverAnswer(body: unknown): unknown {
  if (Array.isArray(body)) return body.map(serverAnswer);
  const { query } = body as { query: string };
  if (query.includes("c: configuration { u: ui }")) {
    return { data: { c: { u: serverUI } } };
  }
  if (query.includes("configuration")) {
    return {
      data: {
        configuration: {
          __typename: "ConfigResult",
          ui: serverUI,
          plugins: {},
        },
      },
    };
  }
  if (query.includes("configureInterface")) {
    return {
      data: {
        configureInterface: { language: "en-GB" },
        configureUISetting: serverUI,
      },
    };
  }
  return { data: { findScenes: { count: 3 } } };
}

const fetchMock = vi.fn(
  async (input: RequestInfo | URL, init?: RequestInit) => {
    const req = new Request(input, init);
    const text = await req.text();
    let body: unknown;
    try {
      body = JSON.parse(text);
    } catch {
      serverRequests.push({ headers: req.headers, body: text });
      return new Response("bad request", { status: 400 });
    }
    serverRequests.push({ headers: req.headers, body });
    return new Response(JSON.stringify(serverAnswer(body)), {
      status: 200,
      headers: { "Content-Type": "application/json; charset=utf-8" },
    });
  }
);

function post(
  body: unknown,
  headers: Record<string, string> = {},
  url = GRAPHQL
): IFakeFetchEvent {
  const event: IFakeFetchEvent = {
    request: new Request(url, {
      method: "POST",
      headers: { "Content-Type": "application/json", ...headers },
      body: typeof body === "string" ? body : JSON.stringify(body),
    }),
    clientId: "client-1",
    respondWith(r) {
      this.response = r;
    },
  };
  listeners.fetch(event);
  return event;
}

async function json(event: IFakeFetchEvent) {
  expect(event.response).toBeDefined();
  const res = await event.response!;
  return res.json();
}

const configQuery = {
  operationName: "Configuration",
  query: `query Configuration { configuration { ...ConfigData } }
    fragment ConfigData on ConfigResult { ui plugins }`,
  variables: {},
};

beforeAll(async () => {
  const g = globalThis as Record<string, unknown>;
  g.registration = { scope: SCOPE };
  g.clients = { claim: async () => {} };
  g.skipWaiting = async () => {};
  g.addEventListener = (type: string, l: Listener) => {
    listeners[type] = l;
  };
  g.fetch = fetchMock;
  g.BroadcastChannel = class {
    // eslint-disable-next-line class-methods-use-this
    postMessage(m: unknown) {
      broadcasts.push(m);
    }
  };
  await import("./sw");
});

beforeEach(() => {
  fetchMock.mockClear();
  serverRequests.length = 0;
  broadcasts.length = 0;
});

describe("service worker", () => {
  it("ignores unrelated requests", () => {
    expect(post({}, {}, `${SCOPE}other`).response).toBeUndefined();
    expect(
      post(configQuery, { "X-Stash-UI-Sync": "1" }).response
    ).toBeUndefined();
    expect(
      post("--x", { "Content-Type": "multipart/form-data; boundary=x" })
        .response
    ).toBeUndefined();
  });

  it("seeds IndexedDB from the server on the first configuration query", async () => {
    serverUI = { theme: "dark" };
    const out = await json(post(configQuery));
    expect(out.data.configuration.ui).toEqual({ theme: "dark" });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("answers configureUISetting locally", async () => {
    const out = await json(
      post({
        operationName: "ConfigureUISetting",
        query:
          "mutation ConfigureUISetting($key: String!, $value: Any) { configureUISetting(key: $key, value: $value) }",
        variables: {
          key: "taggerConfig",
          value: { fingerprintQueue: { "https://stashdb.org": ["1"] } },
        },
      })
    );
    const expected = {
      theme: "dark",
      taggerConfig: { fingerprintQueue: { "https://stashdb.org": ["1"] } },
    };
    expect(out).toEqual({ data: { configureUISetting: expected } });
    expect(fetchMock).not.toHaveBeenCalled();
    expect(broadcasts).toEqual([
      expect.objectContaining({
        type: "stash-ui-config:updated",
        ui: expected,
        sourceClientId: "client-1",
      }),
    ]);
  });

  it("serves the IndexedDB value instead of the server's", async () => {
    serverUI = { theme: "server" };
    const out = await json(
      post({ query: "{ c: configuration { u: ui } }", variables: {} })
    );
    expect(out.data.c.u).toEqual({
      theme: "dark",
      taggerConfig: { fingerprintQueue: { "https://stashdb.org": ["1"] } },
    });
  });

  it("forwards unrelated GraphQL operations unchanged", async () => {
    const out = await json(post({ query: "{ findScenes { count } }" }));
    expect(out).toEqual({ data: { findScenes: { count: 3 } } });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("forwards mixed mutations and applies the UI part locally", async () => {
    const out = await json(
      post({
        query: `mutation { configureInterface(input: {}) { language }
          configureUISetting(key: "mixed", value: true) }`,
      })
    );
    expect(serverRequests).toHaveLength(1);
    expect(out.data.configureInterface).toEqual({ language: "en-GB" });
    expect(out.data.configureUISetting.mixed).toBe(true);
    expect(out.data.configureUISetting.theme).toBe("dark");
  });

  it("handles batched requests", async () => {
    const out = await json(
      post([
        { query: 'mutation { configureUI(partial: {theme: "light"}) }' },
        configQuery,
        { query: "{ findScenes { count } }" },
      ])
    );
    // only the two non-local operations reach the server, as a batch
    expect(serverRequests).toHaveLength(1);
    expect(serverRequests[0].body).toEqual([
      configQuery,
      { query: "{ findScenes { count } }" },
    ]);
    expect(out).toHaveLength(3);
    expect(out[0].data.configureUI.theme).toBe("light");
    expect(out[1].data.configuration.ui.theme).toBe("light");
    expect(out[2]).toEqual({ data: { findScenes: { count: 3 } } });
  });

  it("forwards unparsable bodies to the server", async () => {
    const res = await post("{ not json configuration").response;
    expect(res).toBeDefined();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});
