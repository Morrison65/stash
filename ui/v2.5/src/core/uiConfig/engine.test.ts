import { describe, expect, it } from "vitest";
import { parse } from "graphql";
import { IUIConfigUpdatedMessage, UIConfigEngine } from "./engine";
import { analyzeOperation, UIConfigMap } from "./operations";
import { IStoredUIConfig } from "./store";

function memoryEngine(initial?: UIConfigMap, serverUI?: UIConfigMap) {
  let stored: IStoredUIConfig | undefined = initial
    ? { ui: initial, updatedAt: 1 }
    : undefined;
  const messages: IUIConfigUpdatedMessage[] = [];
  let serverFetches = 0;
  const engine = new UIConfigEngine({
    fetchServerUI: async () => {
      serverFetches += 1;
      return serverUI;
    },
    notify: (m) => messages.push(m),
    getStored: async () => stored,
    seed: async (ui) => {
      if (!stored) stored = { ui, updatedAt: 2 };
      return stored.ui;
    },
    update: async (fn) => {
      const out = fn(stored?.ui ?? {});
      stored = { ui: out.ui, updatedAt: 3 };
      return { ...out, updatedAt: 3 };
    },
  });
  return {
    engine,
    messages,
    getStored: () => stored,
    getServerFetches: () => serverFetches,
  };
}

const configQuery = parse("query { configuration { ui plugins } }");
const queryPlan = analyzeOperation(configQuery);
const targets = queryPlan.kind === "query" ? queryPlan.targets : [];

describe("UIConfigEngine", () => {
  it("seeds IndexedDB from the first server response", async () => {
    const m = memoryEngine();
    const out = await m.engine.rewriteQuery(
      { data: { configuration: { ui: { s: 1 }, plugins: {} } } },
      targets
    );
    expect(out.data).toEqual({ configuration: { ui: { s: 1 }, plugins: {} } });
    expect(m.getStored()?.ui).toEqual({ s: 1 });
  });

  it("serves the stored value instead of the server's", async () => {
    const m = memoryEngine({ local: true });
    const out = await m.engine.rewriteQuery(
      { data: { configuration: { ui: { s: 1 }, plugins: {} } } },
      targets
    );
    expect(out.data).toEqual({
      configuration: { ui: { local: true }, plugins: {} },
    });
  });

  it("answers UI mutations locally and notifies", async () => {
    const m = memoryEngine({ a: 1 });
    const doc = parse(
      'mutation M($v: Any) { configureUISetting(key: "b.c", value: $v) }'
    );
    const plan = analyzeOperation(doc);
    if (plan.kind !== "local") throw new Error(plan.kind);
    const out = await m.engine.localMutationResult(
      doc,
      "M",
      plan.fields,
      { v: 2 },
      "tab-1"
    );
    expect(out).toEqual({
      data: { configureUISetting: { a: 1, b: { c: 2 } } },
    });
    expect(m.getStored()?.ui).toEqual({ a: 1, b: { c: 2 } });
    expect(m.messages).toEqual([
      {
        type: "stash-ui-config:updated",
        ui: { a: 1, b: { c: 2 } },
        updatedAt: 3,
        sourceClientId: "tab-1",
      },
    ]);
    expect(m.getServerFetches()).toBe(0);
  });

  it("seeds from the server before the first local write", async () => {
    const m = memoryEngine(undefined, { fromServer: 1 });
    const doc = parse("mutation { configureUI(partial: {x: 1}) }");
    const plan = analyzeOperation(doc);
    if (plan.kind !== "local") throw new Error(plan.kind);
    await m.engine.localMutationResult(doc, undefined, plan.fields, {});
    expect(m.getServerFetches()).toBe(1);
    expect(m.getStored()?.ui).toEqual({ fromServer: 1, x: 1 });
  });

  it("returns a GraphQL error for invalid arguments without writing", async () => {
    const m = memoryEngine({ a: 1 });
    const doc = parse("mutation { configureUI(input: 5) }");
    const plan = analyzeOperation(doc);
    if (plan.kind !== "local") throw new Error(plan.kind);
    const out = await m.engine.localMutationResult(
      doc,
      undefined,
      plan.fields,
      {}
    );
    expect(out.data).toBeNull();
    expect(out.errors).toHaveLength(1);
    expect(m.getStored()?.ui).toEqual({ a: 1 });
    expect(m.messages).toHaveLength(0);
  });
});
