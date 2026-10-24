# UI configuration in the browser

The free-form UI configuration (`configuration.ui` in GraphQL, written by the
`configureUI(input, partial)` and `configureUISetting(key, value)` mutations)
is stored in the browser's IndexedDB instead of on the server. This includes
the stash-box tagger's `taggerConfig.fingerprintQueue`: queued scenes stay in
the browser and the server only learns about them when the user presses
**Submit fingerprints** (`submitStashBoxFingerprints`).

The GraphQL schema and API are unchanged. The browser answers those
operations itself, so the app and third-party UI plugins keep working
without changes.

## Storage

IndexedDB database `stash-ui`, object store `config`:

| key | value |
|---|---|
| `ui` | the UI configuration map |
| `updatedAt` | time of the last local write (ms since epoch) |

Writes are read-modify-write inside one IndexedDB transaction, so concurrent
writers (tabs) do not lose updates. If nothing is stored
yet (first run, new browser), the store is seeded from the server's
`configuration.ui`.

## Mechanism

Code: `ui/v2.5/src/core/uiConfig/`.

An **Apollo link** in the app's client (`createUIConfigLink`, added in
`createClient.ts` before the transport links) handles the operations of the
app and of UI plugins that use the app's Apollo client:

- **Queries** that select `configuration { ui }` at the root (aliases, named
  or inline fragments, named or anonymous operations): the request is
  forwarded, then every aliased `ui` value inside `configuration` is
  replaced with the IndexedDB value.
- **Mutations whose root fields are only `configureUI` /
  `configureUISetting`** are not forwarded. They are applied locally with the
  server's semantics:
  - `configureUI`: a non-null `input` replaces the whole map, then a non-null
    `partial` is deep-merged into it (`utils.MergeMaps`).
  - `configureUISetting`: the dotted key is set the way
    `utils.NestedMap.Set` does it. A `null` value is stored as `null`; the
    key is not deleted.

  Fields run in document order. Each field (by alias) returns the map as it
  is after that field.
- **Mutations that mix** those fields with others are forwarded unchanged.
  The UI part is also applied locally, and those fields in the response are
  replaced with the local result.
- Operations with the header `X-Stash-UI-Sync: 1` (in the Apollo context) are
  passed through untouched.
- After each write, the change is posted on the BroadcastChannel
  `stash-ui-config`, and other open tabs update their Apollo cache.

Without IndexedDB (e.g. some private modes) everything goes to the server, as
in upstream Stash.

### The former service worker (removed 2026-10-24)

Until 2026-10-24 a service worker (`<base>sw.js`) did the same for every
`POST /graphql` of the page, including plugins' raw `fetch` calls, and the
link only stood in when no worker controlled the page. The worker put itself
in the path of every GraphQL request and added latency (up to seconds,
growing over a page's lifetime), so it was removed:

- the app no longer registers it and unregisters an existing registration on
  load (`removeServiceWorker` in `client.ts`);
- `ui/v2.5/public/sw.js` is now a script that removes itself: browsers that
  still have the old worker fetch it on their next update check, it takes
  over at once (no fetch handler) and unregisters. The gateway still serves
  it with `Cache-Control: no-cache`. Both can be dropped once no browser has
  the old worker any more.

## Troubleshooting

**Settings → Tools → Troubleshooting** shows whether the configuration is
stored in this browser and when it was last updated. It
has two buttons:

- **Sync UI configuration to server** sends the stored map with
  `configureUI(input: …)` and the bypass header, so the request really
  reaches the server (as a backup, or to move the configuration to another
  browser).
- **Load UI configuration from server** asks for confirmation, then
  overwrites IndexedDB with the server's `configuration.ui` and reloads.

## Deployment requirements

None beyond serving the UI: the link runs in the page, so it works over
plain http on any address. (The CSP still allows `worker-src 'self'` for the
removal script above.)

## Limitations

- **Raw-fetch plugins:** plugins that call `fetch('/graphql')` directly
  bypass the Apollo link. They read and write the server's copy, not the
  browser's.
- **Per-browser configuration:** each browser or profile has its own
  configuration. Use the troubleshooting buttons to copy it.
- **Server-side readers** of the UI config see only the last synced copy.
  For example, `config.GetMinimumPlayPercent()` reads
  `ui.minimumPlayPercent` on the server.
- **Mixed mutations:** the local UI write happens before the request is
  forwarded, so it is kept even if the server rejects the request.
- **Cache refresh across tabs:** another tab's cache is updated only if that
  tab has already loaded the `Configuration` query. Other queries that
  select `ui` pick up the new value on their next fetch.
