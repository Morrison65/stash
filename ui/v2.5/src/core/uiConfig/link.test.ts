// The UI config link, with fake-indexeddb and a mocked terminating link
// standing in for the network.
import "fake-indexeddb/auto";
import { describe, expect, it, vi } from "vitest";
import {
  ApolloLink,
  FetchResult,
  Observable,
  execute,
  from,
  gql,
  toPromise,
} from "@apollo/client/core";
import { createUIConfigLink } from "./link";
import { getStoredUIConfig } from "./store";

const serverUI = { fromServer: true };

const network = vi.fn(
  (operation: { operationName: string }) =>
    ({
      data:
        operation.operationName === "Configuration"
          ? { configuration: { ui: serverUI, plugins: {} } }
          : { configureUISetting: serverUI },
    } as FetchResult)
);

const terminating = new ApolloLink(
  (operation) =>
    new Observable<FetchResult>((observer) => {
      observer.next(network(operation));
      observer.complete();
    })
);

// the first local write seeds from the server via fetch
vi.stubGlobal(
  "fetch",
  vi.fn(
    async () =>
      new Response(
        JSON.stringify({ data: { configuration: { ui: serverUI } } }),
        { headers: { "Content-Type": "application/json" } }
      )
  )
);

const link = from([
  createUIConfigLink({ graphqlURL: "http://stash.test/graphql" }),
  terminating,
]);

describe("createUIConfigLink", () => {
  it("answers UI-only mutations locally", async () => {
    const result = await toPromise(
      execute(link, {
        query: gql`
          mutation ConfigureUISetting($key: String!, $value: Any) {
            configureUISetting(key: $key, value: $value)
          }
        `,
        variables: { key: "a.b", value: 1 },
      })
    );
    expect(result.data).toEqual({
      configureUISetting: { fromServer: true, a: { b: 1 } },
    });
    expect(network).not.toHaveBeenCalled();
    expect((await getStoredUIConfig())?.ui).toEqual({
      fromServer: true,
      a: { b: 1 },
    });
  });

  it("rewrites configuration.ui with the stored value", async () => {
    const result = await toPromise(
      execute(link, {
        query: gql`
          query Configuration {
            configuration {
              ui
              plugins
            }
          }
        `,
      })
    );
    expect(network).toHaveBeenCalledTimes(1);
    expect(result.data).toEqual({
      configuration: { ui: { fromServer: true, a: { b: 1 } }, plugins: {} },
    });
  });

  it("passes requests with the bypass header to the network", async () => {
    network.mockClear();
    const result = await toPromise(
      execute(link, {
        query: gql`
          mutation ConfigureUISetting($key: String!, $value: Any) {
            configureUISetting(key: $key, value: $value)
          }
        `,
        variables: { key: "x", value: 2 },
        context: { headers: { "X-Stash-UI-Sync": "1" } },
      })
    );
    expect(network).toHaveBeenCalledTimes(1);
    expect(result.data).toEqual({ configureUISetting: serverUI });
    expect((await getStoredUIConfig())?.ui).toEqual({
      fromServer: true,
      a: { b: 1 },
    });
  });
});
