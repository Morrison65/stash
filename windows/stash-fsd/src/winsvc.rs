//! Windows service registration and the service entrypoint.

use std::ffi::OsString;
use std::path::PathBuf;
use std::sync::{Arc, OnceLock};
use std::time::{Duration, Instant};

use anyhow::{Context, Result};
use tokio::sync::Notify;
use windows_service::service::{
    ServiceAccess, ServiceAction, ServiceActionType, ServiceControl, ServiceControlAccept, ServiceErrorControl,
    ServiceExitCode, ServiceFailureActions, ServiceFailureResetPeriod, ServiceInfo, ServiceStartType, ServiceState,
    ServiceStatus, ServiceType,
};
use windows_service::service_control_handler::{self, ServiceControlHandlerResult, ServiceStatusHandle};
use windows_service::service_manager::{ServiceManager, ServiceManagerAccess};
use windows_service::{define_windows_service, service_dispatcher};

use crate::config::Config;

pub const SERVICE_NAME: &str = "stash-fsd";
const DISPLAY_NAME: &str = "Stash filesystem service (stash-fsd)";
const DESCRIPTION: &str = "Serves the Stash media library to the Stash containers over gRPC and HTTP.";

static CONFIG_PATH: OnceLock<Option<PathBuf>> = OnceLock::new();

define_windows_service!(ffi_service_main, service_main);

/// Entry point used by the SCM (`stash-fsd.exe service [--config ...]`).
pub fn run_dispatcher(config: Option<PathBuf>) -> Result<()> {
    let _ = CONFIG_PATH.set(config);
    service_dispatcher::start(SERVICE_NAME, ffi_service_main).context("starting service dispatcher (is this running under the SCM?)")
}

fn service_main(_args: Vec<OsString>) {
    if let Err(e) = run_service() {
        tracing::error!("service failed: {e:#}");
    }
}

fn set_status(h: &ServiceStatusHandle, state: ServiceState, exit: ServiceExitCode, wait: Duration) {
    let controls = if state == ServiceState::Running {
        ServiceControlAccept::STOP | ServiceControlAccept::SHUTDOWN
    } else {
        ServiceControlAccept::empty()
    };
    let _ = h.set_service_status(ServiceStatus {
        service_type: ServiceType::OWN_PROCESS,
        current_state: state,
        controls_accepted: controls,
        exit_code: exit,
        checkpoint: 0,
        wait_hint: wait,
        process_id: None,
    });
}

fn run_service() -> Result<()> {
    let stop = Arc::new(Notify::new());
    let stop2 = stop.clone();
    let handler = move |ev| match ev {
        ServiceControl::Stop | ServiceControl::Shutdown => {
            stop2.notify_one();
            ServiceControlHandlerResult::NoError
        }
        ServiceControl::Interrogate => ServiceControlHandlerResult::NoError,
        _ => ServiceControlHandlerResult::NotImplemented,
    };
    let h = service_control_handler::register(SERVICE_NAME, handler)?;
    set_status(&h, ServiceState::StartPending, ServiceExitCode::Win32(0), Duration::from_secs(10));

    let explicit = CONFIG_PATH.get().cloned().flatten();
    let result = (|| -> Result<()> {
        let (cfg, path) = Config::load_effective(explicit.as_deref())?;
        let _guard = crate::logging::init(&cfg, false);
        tracing::info!(config = ?path, "starting as Windows service");
        let rt = tokio::runtime::Builder::new_multi_thread().enable_all().build()?;
        set_status(&h, ServiceState::Running, ServiceExitCode::Win32(0), Duration::ZERO);
        let res = rt.block_on(crate::server::run(cfg, async move { stop.notified().await }));
        if let Err(e) = &res {
            tracing::error!("server error: {e:#}");
        }
        rt.shutdown_timeout(Duration::from_secs(10));
        res
    })();

    let exit = if result.is_ok() { ServiceExitCode::Win32(0) } else { ServiceExitCode::ServiceSpecific(1) };
    set_status(&h, ServiceState::Stopped, exit, Duration::ZERO);
    result
}

pub fn install(config: Option<PathBuf>) -> Result<()> {
    let config = match config {
        Some(c) => {
            let abs = std::path::absolute(&c)?;
            Config::load(&abs)?.validate().context("config is not valid")?;
            Some(abs)
        }
        None => {
            if let (cfg, Some(p)) = Config::load_effective(None)? {
                cfg.validate().with_context(|| format!("config {} is not valid", p.display()))?;
            } else {
                eprintln!("warning: no config file found yet; the service will look it up at start");
            }
            None
        }
    };

    let manager = ServiceManager::local_computer(None::<&str>, ServiceManagerAccess::CONNECT | ServiceManagerAccess::CREATE_SERVICE)
        .context("connecting to the service manager (run as administrator)")?;
    let mut launch_arguments = vec![OsString::from("service")];
    if let Some(c) = &config {
        launch_arguments.push("--config".into());
        launch_arguments.push(c.clone().into_os_string());
    }
    let info = ServiceInfo {
        name: SERVICE_NAME.into(),
        display_name: DISPLAY_NAME.into(),
        service_type: ServiceType::OWN_PROCESS,
        start_type: ServiceStartType::AutoStart,
        error_control: ServiceErrorControl::Normal,
        executable_path: std::env::current_exe()?,
        launch_arguments,
        dependencies: vec![],
        account_name: None, // LocalSystem
        account_password: None,
    };
    let service = manager
        .create_service(&info, ServiceAccess::CHANGE_CONFIG | ServiceAccess::START)
        .context("creating service")?;
    service.set_description(DESCRIPTION)?;
    let restart = ServiceAction {
        action_type: ServiceActionType::Restart,
        delay: Duration::from_secs(10),
    };
    service.update_failure_actions(ServiceFailureActions {
        reset_period: ServiceFailureResetPeriod::After(Duration::from_secs(24 * 3600)),
        reboot_msg: None,
        command: None,
        actions: Some(vec![restart.clone(), restart.clone(), restart]),
    })?;
    println!("service {SERVICE_NAME} installed (automatic start)");
    println!("start it with: sc.exe start {SERVICE_NAME}");
    Ok(())
}

pub fn uninstall() -> Result<()> {
    let manager = ServiceManager::local_computer(None::<&str>, ServiceManagerAccess::CONNECT)
        .context("connecting to the service manager (run as administrator)")?;
    let service = manager
        .open_service(SERVICE_NAME, ServiceAccess::QUERY_STATUS | ServiceAccess::STOP | ServiceAccess::DELETE)
        .context("opening service")?;
    if service.query_status()?.current_state != ServiceState::Stopped {
        let _ = service.stop();
        let start = Instant::now();
        while service.query_status()?.current_state != ServiceState::Stopped && start.elapsed() < Duration::from_secs(30) {
            std::thread::sleep(Duration::from_millis(250));
        }
    }
    service.delete().context("deleting service")?;
    println!("service {SERVICE_NAME} removed");
    Ok(())
}
