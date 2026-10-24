// Removes the former UI config service worker. Browsers that still have it
// installed fetch this script on their next update check (the gateway serves
// it with Cache-Control: no-cache); it takes over at once, has no fetch
// handler, and unregisters itself. The UI configuration is handled in the
// page (src/core/uiConfig/link.ts), which also unregisters the worker.
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (event) => {
  event.waitUntil(self.registration.unregister());
});
