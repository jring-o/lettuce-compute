#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::path::Path;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use tauri::Manager;

use lettuce_compute_desktop_lib::api;
use lettuce_compute_desktop_lib::autostart;
use lettuce_compute_desktop_lib::commands;
use lettuce_compute_desktop_lib::container_runtime;
use lettuce_compute_desktop_lib::notifications;
use lettuce_compute_desktop_lib::sidecar;
use lettuce_compute_desktop_lib::tray;
use lettuce_compute_desktop_lib::updater;
use lettuce_compute_desktop_lib::viz;

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

fn main() {
    // Shared state: the current viz bundle directory. Updated by the frontend via a command.
    let viz_base: Arc<Mutex<Option<String>>> = Arc::new(Mutex::new(None));
    let viz_base_proto = viz_base.clone();

    tauri::Builder::default()
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
            None,
        ))
        .plugin(tauri_plugin_updater::Builder::new().build())
        .plugin(tauri_plugin_opener::init())
        .invoke_handler(tauri::generate_handler![
            commands::get_daemon_info,
            commands::is_initialized,
            commands::run_init,
            commands::quit_app,
            commands::is_autostart_enabled,
            commands::set_autostart,
            commands::regenerate_keypair,
            commands::check_update,
            commands::install_update,
            commands::get_container_runtime_status,
            commands::setup_container_runtime,
            commands::start_container_runtime,
            commands::stop_container_runtime,
            commands::check_podman_prerequisites,
            commands::install_podman,
            commands::get_system_memory_mb,
            commands::test_server_connection,
            commands::fetch_head_info,
            viz::viz_read_file,
            viz::viz_list_files,
            viz::set_viz_base,
        ])
        .setup(|app| {
            let app_handle = app.handle().clone();

            // Set up system tray
            let (tray_icon, tray_items) =
                tray::setup_tray(&app_handle).expect("Failed to set up system tray");

            // Set up auto-start (enables on first launch)
            autostart::setup_autostart(&app_handle);

            // Start sidecar if already initialized
            let needs_wizard = sidecar::ensure_initialized().unwrap_or(true);

            if !needs_wizard {
                if !sidecar::is_daemon_running() {
                    match sidecar::start_sidecar() {
                        Ok(_child) => {
                            // Wait for daemon to become available (non-blocking in background)
                            let handle = app_handle.clone();
                            let tray = tray_icon.clone();
                            std::thread::spawn(move || {
                                if let Ok(info) = sidecar::wait_for_daemon(Duration::from_secs(10))
                                {
                                    tray::start_status_poll(
                                        handle,
                                        tray,
                                        tray_items,
                                    );
                                    // Auto-start container runtime if Podman machine is stopped
                                    container_runtime::ensure_podman_state(&info, "running");
                                }
                            });
                        }
                        Err(e) => {
                            eprintln!("Failed to start sidecar: {}", e);
                        }
                    }
                } else {
                    tray::start_status_poll(app_handle.clone(), tray_icon.clone(), tray_items);
                    // Auto-start container runtime if daemon was already running
                    if let Ok(info) = sidecar::read_daemon_json() {
                        std::thread::spawn(move || {
                            container_runtime::ensure_podman_state(&info, "running");
                        });
                    }
                }

                // Start notification polling (after daemon is available)
                notifications::start_notification_poll(app_handle.clone());
            }

            // Start update checking (runs regardless of daemon state)
            updater::start_update_poll(app_handle.clone());

            // Show window (wizard or main UI)
            if let Some(window) = app.get_webview_window("main") {
                let _ = window.show();
            }

            Ok(())
        })
        .on_window_event(|window, event| {
            // Intercept window close BEFORE the window is destroyed.
            // Hide to tray + pause compute instead of closing.
            if let tauri::WindowEvent::CloseRequested { api, .. } = event {
                api.prevent_close();
                let _ = window.hide();
                // Pause compute so frozen processes use zero CPU.
                if let Ok(info) = sidecar::read_daemon_json() {
                    let client = api::ManagementClient::from_daemon_info(&info);
                    std::thread::spawn(move || {
                        let rt = tokio::runtime::Runtime::new().unwrap();
                        rt.block_on(async {
                            let _ = client.pause().await;
                        });
                    });
                }
            }
        })
        .build(tauri::generate_context!())
        .expect("error while building tauri application")
        .run(|_app, _event| {});
}
