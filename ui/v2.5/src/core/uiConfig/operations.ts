// Pure helpers for the UI config Apollo link (./link.ts). They detect GraphQL
// operations that read or write the free-form UI configuration
// (`configuration.ui`, `configureUI`, `configureUISetting`), apply the
// server's write semantics locally and rewrite responses.
//
// This module stays free of DOM, React and Apollo dependencies so that it can
// be tested on its own.

import {
  ArgumentNode,
  DocumentNode,
  FieldNode,
  FragmentDefinitionNode,
  Kind,
  OperationDefinitionNode,
  SelectionSetNode,
  valueFromASTUntyped,
} from "graphql";

export type UIConfigMap = Record<string, unknown>;
export type Variables = Record<string, unknown> | null | undefined;

export const CONFIGURE_UI = "configureUI";
export const CONFIGURE_UI_SETTING = "configureUISetting";

/** Requests carrying this header (value "1") bypass the local UI config. */
export const UI_SYNC_HEADER = "X-Stash-UI-Sync";

const uiMutationFieldNames = new Set([CONFIGURE_UI, CONFIGURE_UI_SETTING]);

/** A `configuration` root field selecting one or more `ui` fields. */
export interface IConfigUITarget {
  /** response key (alias or name) of the `configuration` field */
  configKey: string;
  /** response keys (alias or name) of the `ui` fields inside it */
  uiKeys: string[];
}

/** A root `configureUI` / `configureUISetting` mutation field. */
export interface IUIMutationField {
  responseKey: string;
  fieldName: typeof CONFIGURE_UI | typeof CONFIGURE_UI_SETTING;
  args: readonly ArgumentNode[];
}

export type OperationPlan =
  /** unrelated to the UI configuration: forward unchanged */
  | { kind: "passthrough" }
  /** query reading `configuration.ui`: forward, then replace the ui values */
  | { kind: "query"; targets: IConfigUITarget[] }
  /** mutation consisting only of UI config fields: answer locally */
  | { kind: "local"; fields: IUIMutationField[] }
  /** mutation mixing UI config fields with others: forward and apply locally */
  | { kind: "mixed"; fields: IUIMutationField[] };

export interface IExecutionResult {
  data?: Record<string, unknown> | null;
  errors?: unknown[];
  extensions?: Record<string, unknown>;
  [key: string]: unknown;
}

export function isPlainObject(v: unknown): v is Record<string, unknown> {
  if (v === null || typeof v !== "object" || Array.isArray(v)) return false;
  const proto = Object.getPrototypeOf(v);
  return proto === Object.prototype || proto === null;
}

function hasOwn(o: object, k: string) {
  return Object.prototype.hasOwnProperty.call(o, k);
}

function getOwn(o: Record<string, unknown>, k: string): unknown {
  return hasOwn(o, k) ? o[k] : undefined;
}

// Defines an own property, so keys like "__proto__" (valid JSON keys) never
// touch the prototype chain.
function setOwn(o: Record<string, unknown>, k: string, v: unknown) {
  Object.defineProperty(o, k, {
    value: v,
    enumerable: true,
    writable: true,
    configurable: true,
  });
}

/** Deep clones JSON-like values (objects, arrays, primitives). */
export function cloneJSON<T>(v: T): T {
  if (Array.isArray(v)) {
    return v.map((e) => cloneJSON(e)) as unknown as T;
  }
  if (isPlainObject(v)) {
    const ret: Record<string, unknown> = {};
    Object.keys(v).forEach((k) => {
      setOwn(ret, k, cloneJSON(v[k]));
    });
    return ret as T;
  }
  return v;
}

function responseKey(field: FieldNode) {
  return field.alias?.value ?? field.name.value;
}

function getFragments(doc: DocumentNode) {
  const ret = new Map<string, FragmentDefinitionNode>();
  doc.definitions.forEach((d) => {
    if (d.kind === Kind.FRAGMENT_DEFINITION) {
      ret.set(d.name.value, d);
    }
  });
  return ret;
}

/**
 * Returns the operation that would be executed, following the GraphQL
 * spec's GetOperation: by name, or the only operation of the document.
 */
export function getOperation(
  doc: DocumentNode,
  operationName?: string | null
): OperationDefinitionNode | undefined {
  const ops = doc.definitions.filter(
    (d): d is OperationDefinitionNode => d.kind === Kind.OPERATION_DEFINITION
  );
  if (operationName) {
    return ops.find((o) => o.name?.value === operationName);
  }
  return ops.length === 1 ? ops[0] : undefined;
}

/**
 * Flattens a selection set into its fields, expanding fragment spreads and
 * inline fragments (recursively, with cycle protection).
 */
export function collectFields(
  selectionSet: SelectionSetNode | undefined,
  fragments: Map<string, FragmentDefinitionNode>,
  visited: Set<string> = new Set()
): FieldNode[] {
  const ret: FieldNode[] = [];
  if (!selectionSet) return ret;

  selectionSet.selections.forEach((s) => {
    switch (s.kind) {
      case Kind.FIELD:
        ret.push(s);
        break;
      case Kind.INLINE_FRAGMENT:
        ret.push(...collectFields(s.selectionSet, fragments, visited));
        break;
      case Kind.FRAGMENT_SPREAD: {
        const name = s.name.value;
        const frag = fragments.get(name);
        if (frag && !visited.has(name)) {
          const next = new Set(visited);
          next.add(name);
          ret.push(...collectFields(frag.selectionSet, fragments, next));
        }
        break;
      }
      default:
        break;
    }
  });

  return ret;
}

/** Determines how an operation interacts with the UI configuration. */
export function analyzeOperation(
  doc: DocumentNode,
  operationName?: string | null
): OperationPlan {
  const op = getOperation(doc, operationName);
  if (!op) return { kind: "passthrough" };

  const fragments = getFragments(doc);
  const rootFields = collectFields(op.selectionSet, fragments);

  if (op.operation === "query") {
    const byKey = new Map<string, Set<string>>();
    rootFields.forEach((f) => {
      if (f.name.value !== "configuration") return;
      const uiKeys = collectFields(f.selectionSet, fragments)
        .filter((sub) => sub.name.value === "ui")
        .map(responseKey);
      if (uiKeys.length === 0) return;
      const key = responseKey(f);
      const set = byKey.get(key) ?? new Set<string>();
      uiKeys.forEach((k) => set.add(k));
      byKey.set(key, set);
    });

    if (byKey.size === 0) return { kind: "passthrough" };
    return {
      kind: "query",
      targets: Array.from(byKey.entries()).map(([configKey, uiKeys]) => ({
        configKey,
        uiKeys: Array.from(uiKeys),
      })),
    };
  }

  if (op.operation === "mutation") {
    const fields: IUIMutationField[] = [];
    let hasOther = false;
    rootFields.forEach((f) => {
      const name = f.name.value;
      if (uiMutationFieldNames.has(name)) {
        fields.push({
          responseKey: responseKey(f),
          fieldName: name as IUIMutationField["fieldName"],
          args: f.arguments ?? [],
        });
      } else if (name !== "__typename") {
        hasOther = true;
      }
    });

    if (fields.length === 0) return { kind: "passthrough" };
    return { kind: hasOther ? "mixed" : "local", fields };
  }

  return { kind: "passthrough" };
}

/** Resolves a field's arguments (literals and variables) to JS values. */
export function resolveArgs(
  args: readonly ArgumentNode[],
  variables: Variables
): Record<string, unknown> {
  const ret: Record<string, unknown> = {};
  args.forEach((a) => {
    ret[a.name.value] = valueFromASTUntyped(a.value, variables ?? undefined);
  });
  return ret;
}

/**
 * Mirrors utils.MergeMaps (pkg/utils/map.go): merges src into dest in place.
 * Nested maps are merged recursively when both sides are maps, otherwise the
 * src value replaces the dest value (null included - it does not delete).
 */
export function mergeMaps(dest: UIConfigMap, src: UIConfigMap) {
  Object.keys(src).forEach((k) => {
    const v = src[k];
    const d = getOwn(dest, k);
    if (isPlainObject(v) && isPlainObject(d)) {
      mergeMaps(d, v);
      return;
    }
    setOwn(dest, k, cloneJSON(v));
  });
}

/**
 * Mirrors utils.NestedMap.Set (pkg/utils/map.go): sets a dotted key in
 * place, replacing non-map intermediate values with new maps. A null value
 * is stored as null; the key is not deleted.
 */
export function setNested(m: UIConfigMap, key: string, value: unknown) {
  const fields = key.split(".");
  let current = m;
  fields.slice(0, -1).forEach((f) => {
    let v = getOwn(current, f);
    if (!isPlainObject(v)) {
      v = {};
      setOwn(current, f, v);
    }
    current = v as UIConfigMap;
  });
  setOwn(current, fields[fields.length - 1], cloneJSON(value));
}

/**
 * Mirrors mutationResolver.ConfigureUI: `input` (when not null) replaces the
 * whole map, then `partial` (when not null) is deep-merged into the result.
 * Returns a new map; `current` is not modified.
 */
export function applyConfigureUI(
  current: UIConfigMap,
  input: unknown,
  partial: unknown
): UIConfigMap {
  let ret = cloneJSON(current);
  if (input !== null && input !== undefined) {
    if (!isPlainObject(input)) throw new Error("input: expected a map");
    ret = cloneJSON(input);
  }
  if (partial !== null && partial !== undefined) {
    if (!isPlainObject(partial)) throw new Error("partial: expected a map");
    mergeMaps(ret, partial);
  }
  return ret;
}

/**
 * Mirrors mutationResolver.ConfigureUISetting. Returns a new map; `current`
 * is not modified.
 */
export function applyConfigureUISetting(
  current: UIConfigMap,
  key: unknown,
  value: unknown
): UIConfigMap {
  if (typeof key !== "string") throw new Error("key: expected a string");
  const ret = cloneJSON(current);
  setNested(ret, key, value === undefined ? null : value);
  return ret;
}

export interface IUIMutationOutcome {
  /** the UI config after all fields were applied */
  ui: UIConfigMap;
  /** field value per response key: the map as of that field */
  results: Record<string, UIConfigMap>;
}

/**
 * Applies the UI mutation fields sequentially (as GraphQL executes mutation
 * root fields serially). Throws on invalid arguments.
 */
export function applyUIMutationFields(
  current: UIConfigMap,
  fields: IUIMutationField[],
  variables: Variables
): IUIMutationOutcome {
  let ui = cloneJSON(current);
  const results: Record<string, UIConfigMap> = {};
  fields.forEach((f) => {
    const args = resolveArgs(f.args, variables);
    if (f.fieldName === CONFIGURE_UI) {
      ui = applyConfigureUI(ui, args.input, args.partial);
    } else {
      ui = applyConfigureUISetting(ui, args.key, args.value);
    }
    results[f.responseKey] = cloneJSON(ui);
  });
  return { ui, results };
}

/** Builds the response of a mutation answered entirely locally. */
export function buildLocalMutationResult(
  doc: DocumentNode,
  operationName: string | null | undefined,
  results: Record<string, UIConfigMap>
): IExecutionResult {
  const data: Record<string, unknown> = {};
  const op = getOperation(doc, operationName);
  if (op) {
    collectFields(op.selectionSet, getFragments(doc)).forEach((f) => {
      const key = responseKey(f);
      if (f.name.value === "__typename") {
        data[key] = "Mutation";
      } else if (key in results) {
        data[key] = results[key];
      }
    });
  }
  return { data };
}

/** Builds a GraphQL error response for a locally failed mutation. */
export function buildErrorResult(
  message: string,
  path?: string
): IExecutionResult {
  return {
    errors: [{ message, ...(path ? { path: [path] } : {}) }],
    data: null,
  };
}

/** Overwrites the UI mutation fields of a network response. */
export function overrideMutationResult(
  result: IExecutionResult,
  results: Record<string, UIConfigMap>
): IExecutionResult {
  if (!isPlainObject(result.data)) return result;
  const data = { ...result.data };
  Object.keys(results).forEach((k) => {
    data[k] = results[k];
  });
  return { ...result, data };
}

/** Returns the first `configuration.ui` value found in a query result. */
export function extractUI(
  result: IExecutionResult,
  targets: IConfigUITarget[]
): UIConfigMap | undefined {
  const { data } = result;
  if (!isPlainObject(data)) return undefined;
  let ret: UIConfigMap | undefined;
  targets.some((t) => {
    const cfg = data[t.configKey];
    if (!isPlainObject(cfg)) return false;
    const k = t.uiKeys.find((uk) => isPlainObject(getOwn(cfg, uk)));
    if (k === undefined) return false;
    ret = cfg[k] as UIConfigMap;
    return true;
  });
  return ret;
}

/** Replaces every selected `configuration.ui` value with `ui`. */
export function rewriteQueryResult(
  result: IExecutionResult,
  targets: IConfigUITarget[],
  ui: UIConfigMap
): IExecutionResult {
  if (!isPlainObject(result.data)) return result;
  const data = { ...result.data };
  targets.forEach((t) => {
    const cfg = data[t.configKey];
    if (!isPlainObject(cfg)) return;
    const newCfg = { ...cfg };
    t.uiKeys.forEach((k) => {
      if (hasOwn(newCfg, k)) setOwn(newCfg, k, cloneJSON(ui));
    });
    data[t.configKey] = newCfg;
  });
  return { ...result, data };
}
