// Minimal IndexedDB wrapper holding the UI configuration.
// Database "stash-ui", object store "config", keys "ui" (the map) and
// "updatedAt" (ms since epoch). Used from both the service worker and the
// page, so it must not depend on DOM-only APIs besides indexedDB.

import { UIConfigMap, isPlainObject } from "./operations";

export const DB_NAME = "stash-ui";
export const DB_VERSION = 1;
export const STORE_NAME = "config";
export const UI_KEY = "ui";
export const UPDATED_AT_KEY = "updatedAt";

export interface IStoredUIConfig {
  ui: UIConfigMap;
  updatedAt?: number;
}

let dbPromise: Promise<IDBDatabase> | undefined;

function openDB(): Promise<IDBDatabase> {
  if (!dbPromise) {
    dbPromise = new Promise<IDBDatabase>((resolve, reject) => {
      const req = indexedDB.open(DB_NAME, DB_VERSION);
      req.onupgradeneeded = () => {
        if (!req.result.objectStoreNames.contains(STORE_NAME)) {
          req.result.createObjectStore(STORE_NAME);
        }
      };
      req.onsuccess = () => {
        const db = req.result;
        // allow future versions (e.g. a newer SW) to upgrade the schema
        db.onversionchange = () => {
          db.close();
          dbPromise = undefined;
        };
        resolve(db);
      };
      req.onerror = () => reject(req.error);
      req.onblocked = () => reject(new Error("IndexedDB open blocked"));
    }).catch((e) => {
      dbPromise = undefined;
      throw e;
    });
  }
  return dbPromise;
}

function readRecord(
  store: IDBObjectStore,
  cb: (rec: IStoredUIConfig | undefined) => void
) {
  const uiReq = store.get(UI_KEY);
  const atReq = store.get(UPDATED_AT_KEY);
  atReq.onsuccess = () => {
    const ui = uiReq.result;
    if (!isPlainObject(ui)) {
      cb(undefined);
      return;
    }
    const at = atReq.result;
    cb({ ui, updatedAt: typeof at === "number" ? at : undefined });
  };
}

function writeRecord(store: IDBObjectStore, ui: UIConfigMap, now: number) {
  store.put(ui, UI_KEY);
  store.put(now, UPDATED_AT_KEY);
}

/**
 * Runs `fn` inside a single readwrite transaction: the current record is
 * read, `fn` decides synchronously what to write (return no `write` to keep
 * it) and the transaction commits atomically - so concurrent writers (other
 * tabs, the service worker) never lose updates.
 */
function transact<T>(
  fn: (current: IStoredUIConfig | undefined) => {
    write?: UIConfigMap;
    result: T;
  }
): Promise<{ result: T; updatedAt?: number }> {
  return openDB().then(
    (db) =>
      new Promise((resolve, reject) => {
        const tx = db.transaction(STORE_NAME, "readwrite");
        const store = tx.objectStore(STORE_NAME);
        let result: T;
        let updatedAt: number | undefined;
        let failure: unknown;
        readRecord(store, (current) => {
          try {
            const out = fn(current);
            result = out.result;
            updatedAt = current?.updatedAt;
            if (out.write) {
              updatedAt = Date.now();
              writeRecord(store, out.write, updatedAt);
            }
          } catch (e) {
            failure = e;
            tx.abort();
          }
        });
        tx.oncomplete = () => resolve({ result, updatedAt });
        tx.onabort = () => reject(failure ?? tx.error);
        tx.onerror = () => reject(failure ?? tx.error);
      })
  );
}

/** Returns the stored UI config, or undefined if none was stored yet. */
export function getStoredUIConfig(): Promise<IStoredUIConfig | undefined> {
  return openDB().then(
    (db) =>
      new Promise<IStoredUIConfig | undefined>((resolve, reject) => {
        const tx = db.transaction(STORE_NAME, "readonly");
        let rec: IStoredUIConfig | undefined;
        readRecord(tx.objectStore(STORE_NAME), (r) => {
          rec = r;
        });
        tx.oncomplete = () => resolve(rec);
        tx.onabort = () => reject(tx.error);
        tx.onerror = () => reject(tx.error);
      })
  );
}

/**
 * Stores `ui` only if nothing is stored yet (first run in this browser).
 * Returns the stored value: the existing one, or the newly seeded `ui`.
 */
export function seedUIConfig(ui: UIConfigMap): Promise<UIConfigMap> {
  return transact((current) =>
    current ? { result: current.ui } : { write: ui, result: ui }
  ).then((r) => r.result);
}

/** Overwrites the stored UI config. Returns the new updatedAt. */
export function putUIConfig(ui: UIConfigMap): Promise<number> {
  return transact(() => ({ write: ui, result: undefined })).then(
    (r) => r.updatedAt ?? Date.now()
  );
}

/**
 * Atomically updates the stored UI config. `fn` receives the current map
 * (empty if none is stored) and returns the new map plus a result value.
 */
export function updateUIConfig<T>(
  fn: (current: UIConfigMap) => { ui: UIConfigMap; result: T }
): Promise<{ ui: UIConfigMap; result: T; updatedAt: number }> {
  return transact((current) => {
    const out = fn(current?.ui ?? {});
    return { write: out.ui, result: out };
  }).then((r) => ({ ...r.result, updatedAt: r.updatedAt ?? Date.now() }));
}
