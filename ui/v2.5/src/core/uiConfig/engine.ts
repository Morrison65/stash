// Orchestration for the UI config Apollo link (./link.ts): applies UI config
// mutations to IndexedDB, rewrites query results and notifies other tabs.
// Transport specifics (Apollo Observables) live in the caller.

import { DocumentNode } from "graphql";
import {
  IConfigUITarget,
  IExecutionResult,
  IUIMutationField,
  IUIMutationOutcome,
  UIConfigMap,
  Variables,
  applyUIMutationFields,
  buildErrorResult,
  buildLocalMutationResult,
  extractUI,
  isPlainObject,
  rewriteQueryResult,
} from "./operations";
import {
  IStoredUIConfig,
  getStoredUIConfig,
  seedUIConfig,
  updateUIConfig,
} from "./store";

export const CHANNEL_NAME = "stash-ui-config";
export const MESSAGE_UPDATED = "stash-ui-config:updated";

/** Posted on the BroadcastChannel after every local UI config write. */
export interface IUIConfigUpdatedMessage {
  type: typeof MESSAGE_UPDATED;
  ui: UIConfigMap;
  updatedAt: number;
  /** tab id of the tab that caused it */
  sourceClientId?: string;
}

/** Query used to fetch the server's UI config (with the bypass header). */
export const SERVER_UI_QUERY = {
  operationName: "StashUIConfigFromServer",
  query: "query StashUIConfigFromServer { configuration { ui } }",
  variables: {},
};

export function serverUIFromResult(result: unknown): UIConfigMap | undefined {
  if (!isPlainObject(result)) return undefined;
  const { data } = result;
  if (!isPlainObject(data) || !isPlainObject(data.configuration)) {
    return undefined;
  }
  const { ui } = data.configuration;
  return isPlainObject(ui) ? ui : undefined;
}

export interface IEngineDeps {
  /** fetches `configuration.ui` from the server, bypassing local handling */
  fetchServerUI: () => Promise<UIConfigMap | undefined>;
  /** notifies other tabs about a change */
  notify: (msg: IUIConfigUpdatedMessage) => void;
  getStored?: () => Promise<IStoredUIConfig | undefined>;
  seed?: (ui: UIConfigMap) => Promise<UIConfigMap>;
  update?: typeof updateUIConfig;
}

export class UIConfigEngine {
  private deps: Required<IEngineDeps>;

  constructor(deps: IEngineDeps) {
    this.deps = {
      getStored: getStoredUIConfig,
      seed: seedUIConfig,
      update: updateUIConfig,
      ...deps,
    };
  }

  /**
   * Makes sure IndexedDB holds a UI config before it is modified, seeding
   * it from the server on first use so that a write never drops the
   * existing server-side settings.
   */
  private async ensureSeeded() {
    const stored = await this.deps.getStored();
    if (stored) return;
    let serverUI: UIConfigMap | undefined;
    try {
      serverUI = await this.deps.fetchServerUI();
    } catch {
      serverUI = undefined;
    }
    await this.deps.seed(serverUI ?? {});
  }

  /** Applies UI mutation fields to IndexedDB and notifies other tabs. */
  async applyMutation(
    fields: IUIMutationField[],
    variables: Variables,
    sourceClientId?: string
  ): Promise<IUIMutationOutcome> {
    await this.ensureSeeded();
    const out = await this.deps.update((current) => {
      const outcome = applyUIMutationFields(current, fields, variables);
      return { ui: outcome.ui, result: outcome };
    });
    this.deps.notify({
      type: MESSAGE_UPDATED,
      ui: out.ui,
      updatedAt: out.updatedAt,
      sourceClientId,
    });
    return out.result;
  }

  /**
   * Answers a mutation consisting only of UI config fields. Argument errors
   * produce a GraphQL error response; storage errors are thrown so that the
   * caller can fall back to the network.
   */
  async localMutationResult(
    doc: DocumentNode,
    operationName: string | null | undefined,
    fields: IUIMutationField[],
    variables: Variables,
    sourceClientId?: string
  ): Promise<IExecutionResult> {
    try {
      // validate arguments before touching storage
      applyUIMutationFields({}, fields, variables);
    } catch (e) {
      return buildErrorResult(
        e instanceof Error ? e.message : String(e),
        fields[0]?.responseKey
      );
    }
    const outcome = await this.applyMutation(fields, variables, sourceClientId);
    return buildLocalMutationResult(doc, operationName, outcome.results);
  }

  /**
   * Replaces the `configuration.ui` values of a query result with the
   * IndexedDB value; seeds IndexedDB from the result on first use.
   */
  async rewriteQuery(
    result: IExecutionResult,
    targets: IConfigUITarget[]
  ): Promise<IExecutionResult> {
    const stored = await this.deps.getStored();
    if (stored) return rewriteQueryResult(result, targets, stored.ui);

    const serverUI = extractUI(result, targets);
    if (!serverUI) return result;
    const ui = await this.deps.seed(serverUI);
    return rewriteQueryResult(result, targets, ui);
  }
}
