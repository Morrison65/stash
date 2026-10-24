import React, { useCallback, useEffect, useState } from "react";
import { Button } from "react-bootstrap";
import { FormattedMessage, useIntl } from "react-intl";
import { faTriangleExclamation } from "@fortawesome/free-solid-svg-icons";
import { Setting } from "./Inputs";
import { SettingSection } from "./SettingSection";
import { ModalComponent } from "src/components/Shared/Modal";
import { useToast } from "src/hooks/Toast";
import {
  IUIConfigStatus,
  getUIConfigStatus,
  loadUIConfigFromServer,
  syncUIConfigToServer,
} from "src/core/uiConfig/client";

const modeMessageIDs: Record<IUIConfigStatus["mode"], string> = {
  "service-worker":
    "config.tools.troubleshooting.ui_config.mode_service_worker",
  "in-page": "config.tools.troubleshooting.ui_config.mode_in_page",
  unavailable: "config.tools.troubleshooting.ui_config.mode_unavailable",
};

// The UI configuration lives in this browser's IndexedDB; these tools copy
// it to or from the server on demand (see docs/architecture/ui-config.md).
export const SettingsUIConfigTroubleshooting: React.FC = () => {
  const intl = useIntl();
  const Toast = useToast();
  const [status, setStatus] = useState<IUIConfigStatus>();
  const [busy, setBusy] = useState(false);
  const [confirmLoad, setConfirmLoad] = useState(false);

  const refreshStatus = useCallback(() => {
    getUIConfigStatus()
      .then(setStatus)
      .catch(() => setStatus({ mode: "unavailable" }));
  }, []);

  useEffect(() => {
    refreshStatus();
    const sw = navigator.serviceWorker;
    sw?.addEventListener("controllerchange", refreshStatus);
    return () => sw?.removeEventListener("controllerchange", refreshStatus);
  }, [refreshStatus]);

  async function onSync() {
    setBusy(true);
    try {
      await syncUIConfigToServer();
      Toast.success(
        intl.formatMessage({
          id: "config.tools.troubleshooting.ui_config.sync_success",
        })
      );
    } catch (e) {
      Toast.error(e);
    } finally {
      setBusy(false);
    }
  }

  async function onLoad() {
    setBusy(true);
    try {
      await loadUIConfigFromServer();
      window.location.reload();
    } catch (e) {
      Toast.error(e);
      setBusy(false);
      setConfirmLoad(false);
    }
  }

  function renderStatus() {
    if (!status) return null;
    return (
      <>
        <FormattedMessage id={modeMessageIDs[status.mode]} />
        <br />
        <FormattedMessage
          id="config.tools.troubleshooting.ui_config.last_updated"
          values={{
            time: status.updatedAt
              ? intl.formatDate(status.updatedAt, {
                  dateStyle: "medium",
                  timeStyle: "medium",
                })
              : intl.formatMessage({
                  id: "config.tools.troubleshooting.ui_config.never",
                }),
          }}
        />
      </>
    );
  }

  return (
    <SettingSection headingID="config.tools.troubleshooting.heading">
      <Setting
        id="ui-config-storage"
        headingID="config.tools.troubleshooting.ui_config.storage"
        subHeading={renderStatus()}
      />
      <Setting
        id="ui-config-sync-to-server"
        headingID="config.tools.troubleshooting.ui_config.sync_to_server"
        subHeadingID="config.tools.troubleshooting.ui_config.sync_to_server_desc"
      >
        <Button
          variant="secondary"
          disabled={busy || status?.mode === "unavailable"}
          onClick={() => onSync()}
        >
          <FormattedMessage id="config.tools.troubleshooting.ui_config.sync_to_server" />
        </Button>
      </Setting>
      <Setting
        id="ui-config-load-from-server"
        headingID="config.tools.troubleshooting.ui_config.load_from_server"
        subHeadingID="config.tools.troubleshooting.ui_config.load_from_server_desc"
      >
        <Button
          variant="danger"
          disabled={busy || status?.mode === "unavailable"}
          onClick={() => setConfirmLoad(true)}
        >
          <FormattedMessage id="config.tools.troubleshooting.ui_config.load_from_server" />
        </Button>
      </Setting>
      <ModalComponent
        show={confirmLoad}
        icon={faTriangleExclamation}
        header={intl.formatMessage({
          id: "config.tools.troubleshooting.ui_config.load_from_server",
        })}
        accept={{
          text: intl.formatMessage({ id: "actions.confirm" }),
          variant: "danger",
          onClick: () => onLoad(),
        }}
        cancel={{ onClick: () => setConfirmLoad(false) }}
        onHide={() => setConfirmLoad(false)}
        isRunning={busy}
      >
        <p>
          <FormattedMessage id="config.tools.troubleshooting.ui_config.load_from_server_confirm" />
        </p>
      </ModalComponent>
    </SettingSection>
  );
};
