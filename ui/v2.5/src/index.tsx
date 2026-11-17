import { ApolloProvider } from "@apollo/client";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import { App } from "./App";
import { getClient } from "./core/StashService";
import { baseURL, getPlatformURL } from "./core/createClient";
import "./index.scss";
import { removeServiceWorker, startUIConfigSync } from "./core/uiConfig/client";

const root = createRoot(document.getElementById("root")!);

// commit the initial render synchronously, matching ReactDOM.render, before
// the custom javascript is loaded. Lazy-loaded routes and content that waits on
// queries still mount later, so custom javascript cannot rely on them existing.
flushSync(() => {
  root.render(
    <>
      <link
        rel="stylesheet"
        type="text/css"
        href={getPlatformURL("css").toString()}
      />
      <BrowserRouter basename={baseURL}>
        <ApolloProvider client={getClient()}>
          <App />
        </ApolloProvider>
      </BrowserRouter>
    </>
  );
});

const script = document.createElement("script");
script.src = getPlatformURL("javascript").toString();
document.body.appendChild(script);

// The UI configuration is kept in IndexedDB by an Apollo link (see
// docs/architecture/ui-config.md); the service worker that did it before is
// removed.
removeServiceWorker();
startUIConfigSync(getClient());
