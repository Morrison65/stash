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
writers (tabs, the service worker) do not lose updates. If nothing is stored
yet (first run, new browser), the store is seeded from the server's
`configuration.ui`.

## Mechanisms

Code: `ui/v2.5/src/core/uiConfig/` (shared) and `ui/v2.5/src/sw/sw.ts`.

1. **Service worker** (`<base>sw.js`, scope `<base>`, registered from
   `src/index.tsx` in production builds). It intercepts JSON
   `POST <base>graphql` requests from the app and from plugins, whether they
   use the app's Apollo client or raw `fetch`. Single and batched (array)
   bodies are supported. Multipart uploads, GET requests and websocket
   traffic are not touched.
   - **Queries** that select `configuration { ui }` at the root (aliases,
     named or inline fragments, named or anonymous operations): the request
     is forwarded, then every aliased `ui` value inside `configuration` is
     replaced with the IndexedDB value.
   - **Mutations whose root fields are only `configureUI` /
     `configureUISetting`** are not forwarded. They are applied locally with
     the server's semantics:
     - `configureUI`: a non-null `input` replaces the whole map, then a
       non-null `partial` is deep-merged into it (`utils.MergeMaps`).
     - `configureUISetting`: the dotted key is set the way
       `utils.NestedMap.Set` does it. A `null` value is stored as `null`; the
       key is not deleted.

     Fields run in document order. Each field (by alias) returns the map as
     it is after that field.
   - **Mutations that mix** those fields with others are forwarded
     unchanged. The UI part is also applied locally, and those fields in the
     response are replaced with the local result.
   - Requests with the header `X-Stash-UI-Sync: 1` are passed through
     untouched.
   - After each write, the change is posted on the BroadcastChannel
     `stash-ui-config`, and other open tabs update their Apollo cache.
   - The worker calls `skipWaiting` and `clients.claim`, and has a version
     (`SW_VERSION`). On any internal error it forwards the original request.
2. **In-page fallback** (Apollo link in `createClient.ts`). Service workers
   need a secure context (https or localhost). Over plain http on a LAN
   address, or before the worker controls the page, the same logic runs as
   an Apollo link. The link checks on every request whether a worker
   controls the page, and steps aside if one does.

## Troubleshooting

**Settings → Tools → Troubleshooting** shows where the configuration is
stored (service worker or in-page fallback) and when it was last updated. It
has two buttons:

- **Sync UI configuration to server** sends the stored map with
  `configureUI(input: …)` and the bypass header, so the request really
  reaches the server (as a backup, or to move the configuration to another
  browser).
- **Load UI configuration from server** asks for confirmation, then
  overwrites IndexedDB with the server's `configuration.ui` and reloads.

## Deployment requirements (nginx)

- The CSP must allow the worker script: `worker-src 'self' blob:`. The
  current Go server only sends `worker-src blob:`, which blocks
  registration. The app still works, but only through the in-page fallback.
- Serve `sw.js` with `Cache-Control: no-cache` so that browsers pick up
  updates quickly. It is emitted uncompressed under that stable name at the
  build root, next to `index.html`.
- `sw.js` is served from the app's base path (`<base href>`), so its default
  scope already covers the app and no `Service-Worker-Allowed` header is
  needed. You only need that header if the script is moved to a different
  path than the scope.
- Serve the app over https (or localhost) to get the service worker.

## Limitations

- **In-page fallback and raw-fetch plugins:** without the service worker,
  plugins that call `fetch('/graphql')` directly bypass the Apollo link. They
  read and write the server's copy, not the browser's.
- **Per-browser configuration:** each browser or profile has its own
  configuration. Use the troubleshooting buttons to copy it.
- **Server-side readers** of the UI config see only the last synced copy.
  For example, `config.GetMinimumPlayPercent()` reads
  `ui.minimumPlayPercent` on the server.
- **Mixed mutations:** the local UI write happens before the request is
  forwarded, so it is kept even if the server rejects the request.
- **Dev server:** the Vite dev server (`pnpm start`) does not register the
  service worker, because it talks to the GraphQL server cross-origin. Only
  the in-page fallback is active there.
- **Cache refresh across tabs:** another tab's cache is updated only if that
  tab has already loaded the `Configuration` query. Other queries that
  select `ui` pick up the new value on their next fetch.
