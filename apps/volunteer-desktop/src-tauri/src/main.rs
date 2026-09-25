#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::path::Path;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

use tauri::{AppHandle, Listener, Manager};

use lettuce_compute_desktop_lib::api;
use lettuce_compute_desktop_lib::autostart;
use lettuce_compute_desktop_lib::commands;
use lettuce_compute_desktop_lib::container_runtime;
use lettuce_compute_desktop_lib::logging;
use lettuce_compute_desktop_lib::notifications;
use lettuce_compute_desktop_lib::sidecar;
use lettuce_compute_desktop_lib::tray;
use lettuce_compute_desktop_lib::updater;
use lettuce_compute_desktop_lib::viz;

/// Event the web view emits once the setup wizard has written the install's
/// config — before the daemon is up (see `App.tsx`). Starting the daemon-backed
/// services on it means a fresh install gets its tray status, notifications
/// and container runtime without relaunching the app, and gets them even when
/// the daemon's first start is slow or fails: the polls tolerate an
/// unreachable daemon, so the tray reads "Stopped" until it answers instead
/// of forever (TB-55).
const APP_INITIALIZED_EVENT: &str = "app_initialized";

/// MIME type from file extension.
fn mime_for_path(path: &Path) -> &'static str {
    match path.extension().and_then(|e| e.to_str()) {
        Some("html") => "text/html",
        Some("js" | "mjs") => "application/javascript",
        Some("css") => "text/css",
        Some("json") => "application/json",
        Some("png") => "image/png",
        Some("jpg" | "jpeg") => "image/jpeg",
        Some("svg") => "image/svg+xml",
        Some("wasm") => "application/wasm",
        Some("glb") => "model/gltf-binary",
        Some("gltf") => "model/gltf+json",
        _ => "application/octet-stream",
    }
}

/// Guard so the daemon-backed background tasks start exactly once per process,
/// whether `setup` starts them (already-initialized install) or the wizard's
/// completion event does (fresh install).
static SERVICES_STARTED: AtomicBool = AtomicBool::new(false);

/// Start everything that needs an initialized install: the daemon itself if it
/// is not running, the tray status poll, the notification poll, and the
/// container runtime. Safe to call more than once; only the first call acts.
///
/// The tray and notification polls tolerate an unreachable daemon (they show
/// "Stopped" / skip a tick), so they start immediately; only the container
/// runtime step waits for the daemon to be up, on a background thread.
fn start_daemon_services(app: AppHandle, tray_icon: tauri::tray::TrayIcon, tray_items: tray::TrayMenuItems) {
    if SERVICES_STARTED.swap(true, Ordering::SeqCst) {
        return;
    }

    tray::start_status_poll(app.clone(), tray_icon, tray_items);
    notifications::start_notification_poll(app);

    std::thread::spawn(move || {
        // On a fresh install the wizard's `run_init` has usually started the
        // daemon already; `ensure_daemon_started` then does nothing.
        if let Err(e) = sidecar::ensure_daemon_started() {
            log::warn!("failed to start the volunteer daemon: {e}");
            return;
        }
        match sidecar::wait_for_daemon(sidecar::DAEMON_START_TIMEOUT) {
            Ok(sidecar::DaemonStart::Ready(info)) => {
                log::info!(
                    "the volunteer daemon is up (pid {}, management port {})",
                    info.pid,
                    info.port
                );
                // Auto-start the container runtime if the Podman machine is stopped.
                container_runtime::ensure_podman_state(&info, "running");
            }
            Ok(sidecar::DaemonStart::Starting) => log::warn!(
                "the volunteer daemon is still starting after {:?}",
                sidecar::DAEMON_START_TIMEOUT
            ),
            Err(e) => log::warn!("the volunteer daemon did not come up: {e}"),
        }
    });
}

fn main() {
    // Shared state: the current viz bundle directory. Updated by the frontend via a command.
    let viz_base: Arc<Mutex<Option<String>>> = Arc::new(Mutex::new(None));
    let viz_base_proto = viz_base.clone();

    let launch_minimized = std::env::args()
        .skip(1)
        .any(|a| a == autostart::MINIMIZED_FLAG);

    // Registered first, so every other plugin's start-up is logged too.
    let (log_plugin, log_file_unavailable) = logging::plugin();

    tauri::Builder::default()
        .plugin(log_plugin)
        .manage(viz::VizBaseDir(viz_base))
        .register_uri_scheme_protocol("lettuce-viz", move |_ctx, request| {
            let uri_path = percent_encoding::percent_decode_str(request.uri().path())
                .decode_utf8_lossy()
                .to_string();
            // Strip leading slash.
            let rel = uri_path.trim_start_matches('/');
            let rel = if rel.is_empty() { "index.html" } else { rel };

            // Reject path traversal.
            if rel.contains("..") {
                return http::Response::builder()
                    .status(403)
                    .body(b"Forbidden".to_vec())
                    .unwrap();
            }

            let base = viz_base_proto.lock().unwrap().clone();
            let Some(base_dir) = base else {
                return http::Response::builder()
                    .status(404)
                    .body(b"No viz bundle active".to_vec())
                    .unwrap();
            };

            let file_path = Path::new(&base_dir).join(rel);

            // Verify the resolved path stays within the base directory.
            if let (Ok(canon_base), Ok(canon_file)) =
                (Path::new(&base_dir).canonicalize(), file_path.canonicalize())
            {
                if !canon_file.starts_with(&canon_base) {
                    return http::Response::builder()
                        .status(403)
                        .body(b"Forbidden".to_vec())
                        .unwrap();
                }
            }

            match std::fs::read(&file_path) {
                Ok(data) => {
                    let mime = mime_for_path(&file_path);
                    http::Response::builder()
                        .status(200)
                        .header("Content-Type", mime)
                        .header("Access-Control-Allow-Origin", "*")
                        .body(data)
                        .unwrap()
                }
                Err(_) => http::Response::builder()
                    .status(404)
                    .body(b"Not found".to_vec())
                    .unwrap(),
            }
        })
        .plugin(tauri_plugin_notification::init())
        .plugin(tauri_plugin_autostart::init(
            tauri_plugin_autostart::MacosLauncher::LaunchAgent,
            Some(autostart::launch_args()),
        ))
        .plugin(tauri_plugin_updater::Builder::new().build())
        .plugin(tauri_plugin_opener::init())
        .invoke_handler(tauri::generate_handler![
            api::mgmt_request,
            commands::is_initialized,
            commands::run_init,
            commands::wait_for_daemon,
            commands::get_daemon_process_state,
            commands::restart_daemon,
            commands::get_data_dir,
            commands::get_log_dir,
            commands::open_log_folder,
            commands::log_from_webview,
            commands::get_client_version,
            commands::system_metrics,
            commands::is_autostart_enabled,
            commands::set_autostart,
            commands::regenerate_keypair,
            commands::install_update,
            commands::get_container_runtime_status,
            commands::setup_container_runtime,
            commands::start_container_runtime,
            commands::stop_container_runtime,
            commands::redetect_container_runtime,
            commands::check_podman_prerequisites,
            commands::install_podman,
            commands::detect_container_runtime,
            commands::get_system_memory_mb,
            commands::get_system_cpu_count,
            commands::test_server_connection,
            commands::fetch_head_info,
            viz::viz_read_file,
            viz::viz_list_files,
            viz::set_viz_base,
        ])
        .setup(move |app| {
            let app_handle = app.handle().clone();
            let needs_wizard = sidecar::ensure_initialized().unwrap_or(true);

            log::info!(
                "{}",
                logging::session_start_line(
                    &app.package_info().version.to_string(),
                    std::env::consts::OS,
                    std::env::consts::ARCH,
                    &sidecar::data_dir(),
                    launch_minimized,
                    needs_wizard,
                )
            );
            if let Some(reason) = &log_file_unavailable {
                log::warn!("this session is logged to stderr only: {reason}");
            }

            // Set up system tray
            let (tray_icon, tray_items) =
                tray::setup_tray(&app_handle).expect("Failed to set up system tray");
            tray::start_version_tooltip(&app_handle, tray_icon.clone());

            // Refresh the login entry if it is on. A launch never turns it on:
            // the setup wizard asks, and Settings › General can change it.
            autostart::setup_autostart(&app_handle);

            // Start the daemon-backed services now if this install is already
            // set up, otherwise when the wizard reports it is.
            if needs_wizard {
                let handle = app_handle.clone();
                let icon = tray_icon.clone();
                let items = tray_items.clone();
                app.listen(APP_INITIALIZED_EVENT, move |_event| {
                    start_daemon_services(handle.clone(), icon.clone(), items.clone());
                });
            } else {
                start_daemon_services(app_handle.clone(), tray_icon.clone(), tray_items.clone());
            }

            // Start update checking (runs regardless of daemon state)
            updater::start_update_poll(app_handle.clone());

            // Show the window unless this is a login-time launch of a set-up
            // install; the wizard is always shown, since there is nothing to
            // run until it completes.
            if !launch_minimized || needs_wizard {
                if let Some(window) = app.get_webview_window("main") {
                    let _ = window.show();
                }
            }

            Ok(())
        })
        .on_window_event(|window, event| {
            // Intercept window close BEFORE the window is destroyed: the app
            // keeps running in the tray and computing continues. Stopping is
            // Pause (Overview or tray) or Quit.
            if let tauri::WindowEvent::CloseRequested { api, .. } = event {
                api.prevent_close();
                tray::close_to_tray(window.app_handle());
            }
        })
        .build(tauri::generate_context!())
        .expect("error while building tauri application")
        .run(|_app, event| {
            // Every way out of the app must freeze the daemon's work and stop
            // the daemon, not only the tray's Quit. On macOS the application
            // menu's Quit (⌘Q, and the Dock) terminates the app through
            // `applicationWillTerminate`, which Tauri delivers as this final
            // event and nothing earlier — there is no `ExitRequested` to
            // prevent — so the work happens here, synchronously, before the
            // process is allowed to end (TB-56). The tray's Quit has already
            // done it by the time its `app.exit(0)` arrives here; the call is
            // a no-op then.
            if let tauri::RunEvent::Exit = event {
                if let Err(e) = sidecar::suspend_and_quit_sidecar() {
                    log::warn!("could not suspend the volunteer daemon on exit: {e}");
                }
            }
        });
}
