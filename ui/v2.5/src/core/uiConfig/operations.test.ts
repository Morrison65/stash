import { describe, expect, it } from "vitest";
import { parse } from "graphql";
import {
  analyzeOperation,
  applyConfigureUI,
  applyConfigureUISetting,
  applyUIMutationFields,
  buildLocalMutationResult,
  extractUI,
  IUIMutationField,
  mergeMaps,
  overrideMutationResult,
  rewriteQueryResult,
} from "./operations";

function mutationFields(query: string, operationName?: string) {
  const plan = analyzeOperation(parse(query), operationName);
  if (plan.kind !== "local" && plan.kind !== "mixed") {
    throw new Error(`unexpected plan ${plan.kind}`);
  }
  return plan.fields as IUIMutationField[];
}

describe("analyzeOperation", () => {
  it("passes through unrelated queries", () => {
    expect(analyzeOperation(parse("{ findScenes { count } }"))).toEqual({
      kind: "passthrough",
    });
    expect(
      analyzeOperation(parse("{ configuration { general { databasePath } } }"))
    ).toEqual({ kind: "passthrough" });
  });

  it("detects configuration.ui in an anonymous query", () => {
    expect(analyzeOperation(parse("{ configuration { ui } }"))).toEqual({
      kind: "query",
      targets: [{ configKey: "configuration", uiKeys: ["ui"] }],
    });
  });

  it("detects ui through fragments, inline fragments and aliases", () => {
    const doc = parse(`
      query Configuration {
        cfg: configuration { ...ConfigData }
        ... on Query { configuration { ... on ConfigResult { u2: ui } } }
        other: configuration { general { databasePath } }
      }
      fragment ConfigData on ConfigResult {
        general { databasePath }
        ...UIOnly
        plugins
      }
      fragment UIOnly on ConfigResult { ui myui: ui }
    `);
    expect(analyzeOperation(doc)).toEqual({
      kind: "query",
      targets: [
        { configKey: "cfg", uiKeys: ["ui", "myui"] },
        { configKey: "configuration", uiKeys: ["u2"] },
      ],
    });
  });

  it("detects root fields spread from a fragment on Query", () => {
    const doc = parse(`
      query Q { ...Root }
      fragment Root on Query { configuration { ui } }
    `);
    expect(analyzeOperation(doc).kind).toBe("query");
  });

  it("ignores ui fields below the configuration root", () => {
    const doc = parse("{ findScene(id: 1) { configuration { ui } } }");
    expect(analyzeOperation(doc).kind).toBe("passthrough");
  });

  it("survives fragment cycles", () => {
    const doc = parse(`
      { configuration { ...A } }
      fragment A on ConfigResult { ...B ui }
      fragment B on ConfigResult { ...A }
    `);
    expect(analyzeOperation(doc).kind).toBe("query");
  });

  it("selects the operation by name", () => {
    const doc = parse(`
      query A { configuration { ui } }
      mutation B { configureUISetting(key: "a", value: 1) }
      mutation C { configureGeneral(input: {}) { databasePath } }
    `);
    expect(analyzeOperation(doc, "A").kind).toBe("query");
    expect(analyzeOperation(doc, "B").kind).toBe("local");
    expect(analyzeOperation(doc, "C").kind).toBe("passthrough");
    // ambiguous without a name
    expect(analyzeOperation(doc).kind).toBe("passthrough");
    expect(analyzeOperation(doc, "Missing").kind).toBe("passthrough");
  });

  it("classifies UI-only mutations as local", () => {
    const plan = analyzeOperation(
      parse(`
        mutation ConfigureUI($input: Map, $partial: Map) {
          __typename
          a: configureUI(input: $input, partial: $partial)
          configureUISetting(key: "x", value: 1)
        }
      `)
    );
    expect(plan.kind).toBe("local");
    if (plan.kind === "local") {
      expect(plan.fields.map((f) => [f.responseKey, f.fieldName])).toEqual([
        ["a", "configureUI"],
        ["configureUISetting", "configureUISetting"],
      ]);
    }
  });

  it("classifies mixed mutations", () => {
    const plan = analyzeOperation(
      parse(`
        mutation {
          configureInterface(input: {}) { language }
          configureUISetting(key: "x", value: 1)
        }
      `)
    );
    expect(plan.kind).toBe("mixed");
  });

  it("passes through subscriptions", () => {
    expect(
      analyzeOperation(parse("subscription { loggingSubscribe { message } }"))
        .kind
    ).toBe("passthrough");
  });
});

describe("mergeMaps (utils.MergeMaps)", () => {
  it("merges nested maps and replaces other values", () => {
    const dest = { a: { b: 1, c: 2 }, d: [1, 2], e: "x", f: { g: 1 } };
    mergeMaps(dest, { a: { c: 3, h: 4 }, d: [3], e: { y: 1 }, f: null });
    expect(dest).toEqual({
      a: { b: 1, c: 3, h: 4 },
      d: [3],
      e: { y: 1 },
      f: null,
    });
  });

  it("does not alias src values", () => {
    const src = { a: { b: [1] } };
    const dest: Record<string, unknown> = {};
    mergeMaps(dest, src);
    src.a.b.push(2);
    expect(dest).toEqual({ a: { b: [1] } });
  });

  it("treats __proto__ as a plain key", () => {
    const dest: Record<string, unknown> = {};
    mergeMaps(dest, JSON.parse('{"__proto__": {"polluted": true}}'));
    expect(({} as Record<string, unknown>).polluted).toBeUndefined();
    expect(Object.keys(dest)).toEqual(["__proto__"]);
  });
});

describe("applyConfigureUI", () => {
  const current = { a: 1, nested: { b: 2, c: 3 } };

  it("replaces the whole map with input", () => {
    expect(applyConfigureUI(current, { z: 1 }, null)).toEqual({ z: 1 });
    expect(applyConfigureUI(current, {}, undefined)).toEqual({});
  });

  it("deep-merges partial", () => {
    expect(applyConfigureUI(current, null, { nested: { c: 4 } })).toEqual({
      a: 1,
      nested: { b: 2, c: 4 },
    });
  });

  it("applies input then partial", () => {
    expect(applyConfigureUI(current, { x: { y: 1 } }, { x: { z: 2 } })).toEqual(
      { x: { y: 1, z: 2 } }
    );
  });

  it("leaves the map unchanged without arguments", () => {
    expect(applyConfigureUI(current, null, null)).toEqual(current);
  });

  it("does not modify its arguments", () => {
    const cur = { nested: { b: 2 } };
    applyConfigureUI(cur, null, { nested: { b: 3 } });
    expect(cur).toEqual({ nested: { b: 2 } });
  });

  it("rejects non-map arguments", () => {
    expect(() => applyConfigureUI({}, "x", null)).toThrow();
    expect(() => applyConfigureUI({}, null, [1])).toThrow();
  });
});

describe("applyConfigureUISetting (utils.NestedMap.Set)", () => {
  it("sets top-level and dotted keys", () => {
    expect(applyConfigureUISetting({ a: 1 }, "b", true)).toEqual({
      a: 1,
      b: true,
    });
    expect(
      applyConfigureUISetting({ a: { b: { c: 1, d: 2 } } }, "a.b.c", 5)
    ).toEqual({ a: { b: { c: 5, d: 2 } } });
  });

  it("creates and replaces intermediate maps", () => {
    expect(applyConfigureUISetting({}, "x.y.z", "v")).toEqual({
      x: { y: { z: "v" } },
    });
    expect(applyConfigureUISetting({ x: 5 }, "x.y", 1)).toEqual({
      x: { y: 1 },
    });
  });

  it("stores null instead of deleting the key", () => {
    expect(applyConfigureUISetting({ a: 1, b: 2 }, "a", null)).toEqual({
      a: null,
      b: 2,
    });
    expect(applyConfigureUISetting({ a: 1 }, "a", undefined)).toEqual({
      a: null,
    });
  });

  it("replaces values wholesale (no merge)", () => {
    expect(
      applyConfigureUISetting({ t: { a: 1, b: 2 } }, "t", { a: 3 })
    ).toEqual({ t: { a: 3 } });
  });

  it("requires a string key", () => {
    expect(() => applyConfigureUISetting({}, undefined, 1)).toThrow();
  });
});

describe("applyUIMutationFields", () => {
  it("resolves variables and literals and applies fields in order", () => {
    const fields = mutationFields(`
      mutation M($k: String!, $v: Any, $p: Map) {
        first: configureUISetting(key: $k, value: $v)
        second: configureUI(partial: $p)
        third: configureUISetting(key: "lit.eral", value: {list: [1, 2], s: "x"})
      }
    `);
    const out = applyUIMutationFields({ keep: true }, fields, {
      k: "taggerConfig",
      v: { fingerprintQueue: { e: ["1"] } },
      p: { a: 1 },
    });
    const tagger = { fingerprintQueue: { e: ["1"] } };
    expect(out.results.first).toEqual({ keep: true, taggerConfig: tagger });
    expect(out.results.second).toEqual({
      keep: true,
      taggerConfig: tagger,
      a: 1,
    });
    expect(out.ui).toEqual({
      keep: true,
      taggerConfig: tagger,
      a: 1,
      lit: { eral: { list: [1, 2], s: "x" } },
    });
    expect(out.results.third).toEqual(out.ui);
  });

  it("treats missing variables as null", () => {
    const fields = mutationFields(
      "mutation ($input: Map, $partial: Map) { configureUI(input: $input, partial: $partial) }"
    );
    expect(applyUIMutationFields({ a: 1 }, fields, {}).ui).toEqual({ a: 1 });
    expect(
      applyUIMutationFields({ a: 1 }, fields, { input: { b: 2 } }).ui
    ).toEqual({ b: 2 });
  });
});

describe("responses", () => {
  it("builds a local mutation result respecting aliases and __typename", () => {
    const doc = parse(
      'mutation { __typename x: configureUISetting(key: "a", value: 1) }'
    );
    expect(buildLocalMutationResult(doc, undefined, { x: { a: 1 } })).toEqual({
      data: { __typename: "Mutation", x: { a: 1 } },
    });
  });

  it("overrides UI fields of a network mutation result", () => {
    expect(
      overrideMutationResult(
        { data: { configureInterface: { language: "en" }, ui: { a: 0 } } },
        { ui: { a: 1 } }
      )
    ).toEqual({
      data: { configureInterface: { language: "en" }, ui: { a: 1 } },
    });
    expect(overrideMutationResult({ data: null }, { ui: {} })).toEqual({
      data: null,
    });
  });

  it("rewrites every aliased ui value", () => {
    const targets = [
      { configKey: "cfg", uiKeys: ["ui", "u2"] },
      { configKey: "missing", uiKeys: ["ui"] },
    ];
    const result = {
      data: {
        cfg: { __typename: "ConfigResult", ui: { s: 1 }, u2: { s: 1 }, x: 1 },
        other: 1,
      },
      extensions: { e: 1 },
    };
    const out = rewriteQueryResult(result, targets, { local: true });
    expect(out).toEqual({
      data: {
        cfg: {
          __typename: "ConfigResult",
          ui: { local: true },
          u2: { local: true },
          x: 1,
        },
        other: 1,
      },
      extensions: { e: 1 },
    });
    // input unchanged
    expect(result.data.cfg.ui).toEqual({ s: 1 });
  });

  it("does not add ui keys skipped by directives", () => {
    const out = rewriteQueryResult(
      { data: { configuration: { general: {} } } },
      [{ configKey: "configuration", uiKeys: ["ui"] }],
      { a: 1 }
    );
    expect(out).toEqual({ data: { configuration: { general: {} } } });
  });

  it("extracts the server ui value", () => {
    const targets = [{ configKey: "c", uiKeys: ["skipped", "ui"] }];
    expect(extractUI({ data: { c: { ui: { a: 1 } } } }, targets)).toEqual({
      a: 1,
    });
    expect(extractUI({ data: null }, targets)).toBeUndefined();
    expect(extractUI({ data: { c: null } }, targets)).toBeUndefined();
  });
});
