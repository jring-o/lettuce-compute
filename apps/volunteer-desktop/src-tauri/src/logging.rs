//! The app's own log: `<data-dir>/logs/desktop.log`, beside the daemon's
//! `volunteer.log`, rotated by size. What the app has to say about itself —
//! how each session started, the daemon it spawned or adopted, update checks,
//! the login entry, the window being closed and reopened, and the web view's
//! own errors — is written here as well as to stderr. A release build on
//! Windows has no console, and an app started from the Finder, with `open` or
//! at login has its stderr discarded, so without the file none of it would
//! survive the session.

use std::fs::{self, OpenOptions};
use std::path::{Path, PathBuf};

use log::{Level, LevelFilter};
use tauri::plugin::TauriPlugin;
use tauri::Runtime;
use tauri_plugin_log::{RotationStrategy, Target, TargetKind};
use time::format_description::well_known::Rfc3339;
use time::OffsetDateTime;

use crate::sidecar;

/// The log file's name without its extension (the plugin adds `.log`).
pub const LOG_FILE_STEM: &str = "desktop";

/// The log target of lines forwarded from the web view.
pub const WEBVIEW_TARGET: &str = "webview";

/// Rotate the file at this size and keep this many rotated copies besides
/// the live one: the bounds the daemon puts on `volunteer.log`.
const MAX_FILE_BYTES: u128 = 10 * 1024 * 1024;
const ROTATED_FILES_KEPT: usize = 5;

/// Longest message accepted from the web view; a runaway error loop must not
/// write megabytes per line.
const MAX_WEBVIEW_MESSAGE_CHARS: usize = 4000;

/// The folder both logs live in: `logs` under the app's data directory, so a
/// `LETTUCE_DATA_DIR` profile keeps its own.
pub fn log_dir() -> PathBuf {
    log_dir_in(&sidecar::data_dir())
}

fn log_dir_in(data_dir: &Path) -> PathBuf {
    data_dir.join("logs")
}

/// The app's log file.
pub fn log_file() -> PathBuf {
    log_file_in(&log_dir())
}

fn log_file_in(dir: &Path) -> PathBuf {
    dir.join(format!("{LOG_FILE_STEM}.log"))
}

/// Where the log is written.
#[derive(Debug, PartialEq, Eq)]
enum Sink {
    /// stderr, and `desktop.log` in this folder.
    StderrAndFile(PathBuf),
    /// stderr only, for this reason; it is logged once the logger is up.
    StderrOnly(String),
}

/// The file when its folder can be created, the file opened for appending
/// and the folder listed (rotation lists it); stderr alone otherwise.
///
/// The plugin opens the file while it initialises, and an error there fails
/// the app's `build()`, which would stop the app at launch. Checking first
/// means a profile the app cannot write to costs the log file, never the app.
fn choose_sink(dir: &Path) -> Sink {
    let check = || -> std::io::Result<()> {
        fs::create_dir_all(dir)?;
        OpenOptions::new()
            .create(true)
            .append(true)
            .open(log_file_in(dir))?;
        fs::read_dir(dir)?;
        Ok(())
    };
    match check() {
        Ok(()) => Sink::StderrAndFile(dir.to_path_buf()),
        Err(e) => Sink::StderrOnly(format!("cannot write {}: {e}", log_file_in(dir).display())),
    }
}

/// The log plugin, registered before the other plugins so their start-up is
/// captured too, and the reason the file is missing when it had to be left
/// out. The app's own lines are kept at Info (Debug in a debug build);
/// everything its libraries log is kept at Warn and above.
pub fn plugin<R: Runtime>() -> (TauriPlugin<R>, Option<String>) {
    let own_level = if cfg!(debug_assertions) {
        LevelFilter::Debug
    } else {
        LevelFilter::Info
    };
    let mut targets = vec![Target::new(TargetKind::Stderr)];
    let unavailable = match choose_sink(&log_dir()) {
        Sink::StderrAndFile(path) => {
            targets.push(Target::new(TargetKind::Folder {
                path,
                file_name: Some(LOG_FILE_STEM.into()),
            }));
            None
        }
        Sink::StderrOnly(reason) => Some(reason),
    };
    let plugin = tauri_plugin_log::Builder::new()
        .targets(targets)
        .level(LevelFilter::Warn)
        .level_for("lettuce_compute_desktop", own_level)
        .level_for("lettuce_compute_desktop_lib", own_level)
        .level_for(WEBVIEW_TARGET, own_level)
        .rotation_strategy(RotationStrategy::KeepSome(ROTATED_FILES_KEPT))
        .max_file_size(MAX_FILE_BYTES)
        .format(|out, message, record| {
            out.finish(format_args!(
                "{} [{}] [{}] {}",
                timestamp(),
                record.level(),
                record.target(),
                message
            ))
        })
        .build();
    (plugin, unavailable)
}

/// Now, as RFC 3339 with its UTC offset, like the daemon's own timestamps:
/// local time where the offset can be read, else UTC ("Z"), never a local
/// time that does not say so.
fn timestamp() -> String {
    OffsetDateTime::now_local()
        .unwrap_or_else(|_| OffsetDateTime::now_utc())
        .format(&Rfc3339)
        .unwrap_or_default()
}

/// The first line of every session: which build, on what, with which
/// profile, and how it was started, so each later line can be placed.
pub fn session_start_line(
    app_version: &str,
    os: &str,
    arch: &str,
    data_dir: &Path,
    minimized: bool,
    needs_wizard: bool,
) -> String {
    format!(
        "Lettuce Compute {app_version} starting on {os}/{arch}; data directory {}; {}; {}",
        data_dir.display(),
        if minimized {
            "started with --minimized (the login-time launch), window hidden"
        } else {
            "started with its window shown"
        },
        if needs_wizard {
            "not set up yet, showing the setup wizard"
        } else {
            "already set up"
        }
    )
}

/// The line that follows the session start once the bundled client answers.
pub fn client_version_line(version: &str) -> String {
    format!("bundled client: lettuce-volunteer {version}")
}

fn webview_level(level: &str) -> Level {
    match level {
        "error" => Level::Error,
        "warn" => Level::Warn,
        "debug" => Level::Debug,
        _ => Level::Info,
    }
}

/// Write a line the web view sent (see `src/lib/webview-log.ts`) under the
/// `webview` target, cut to a bounded length.
pub fn log_from_webview(level: &str, message: &str) {
    let message: String = message.chars().take(MAX_WEBVIEW_MESSAGE_CHARS).collect();
    log::log!(target: WEBVIEW_TARGET, webview_level(level), "{message}");
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::ffi::OsString;

    fn scratch(name: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "lettuce-logging-test-{}-{name}",
            std::process::id()
        ));
        let _ = fs::remove_dir_all(&dir);
        dir
    }

    #[test]
    fn the_log_folder_is_logs_under_the_data_directory_also_under_an_override() {
        let home = PathBuf::from(if cfg!(windows) { r"C:\Users\vol" } else { "/home/vol" });
        let default = sidecar::resolve_data_dir(None, Some(home.clone()));
        assert_eq!(log_dir_in(&default), home.join(".lettuce").join("logs"));

        let profile = if cfg!(windows) { r"D:\profiles\second" } else { "/srv/profiles/second" };
        let overridden = sidecar::resolve_data_dir(Some(OsString::from(profile)), Some(home));
        assert_eq!(log_dir_in(&overridden), PathBuf::from(profile).join("logs"));
        assert_eq!(
            log_file_in(&log_dir_in(&overridden)),
            PathBuf::from(profile).join("logs").join("desktop.log")
        );
    }

    #[test]
    fn a_writable_folder_gets_the_file_and_is_created_when_missing() {
        let data = scratch("writable");
        let dir = log_dir_in(&data);
        assert_eq!(choose_sink(&dir), Sink::StderrAndFile(dir.clone()));
        assert!(log_file_in(&dir).is_file(), "the check should leave the file in place");
        let _ = fs::remove_dir_all(&data);
    }

    #[test]
    fn a_logs_path_that_is_a_file_falls_back_to_stderr_and_says_why() {
        let data = scratch("logs-is-a-file");
        fs::create_dir_all(&data).unwrap();
        let dir = log_dir_in(&data);
        fs::write(&dir, "not a folder").unwrap();
        match choose_sink(&dir) {
            Sink::StderrOnly(reason) => assert!(
                reason.contains(&log_file_in(&dir).display().to_string()),
                "the reason should name the file it could not write: {reason}"
            ),
            other => panic!("expected stderr only, got {other:?}"),
        }
        let _ = fs::remove_dir_all(&data);
    }

    #[test]
    fn the_session_start_line_names_the_build_platform_profile_and_launch() {
        let dir = PathBuf::from(if cfg!(windows) { r"D:\p" } else { "/p" });
        let line = session_start_line("2.1.2", "windows", "x86_64", &dir, true, false);
        assert!(line.contains("Lettuce Compute 2.1.2"), "{line}");
        assert!(line.contains("windows/x86_64"), "{line}");
        assert!(line.contains(&dir.display().to_string()), "{line}");
        assert!(line.contains("--minimized"), "{line}");
        assert!(line.contains("already set up"), "{line}");

        let first = session_start_line("2.1.2", "macos", "aarch64", &dir, false, true);
        assert!(first.contains("window shown"), "{first}");
        assert!(first.contains("setup wizard"), "{first}");
        assert!(!first.contains("--minimized"), "{first}");
    }

    #[test]
    fn the_client_version_line_names_the_version() {
        assert_eq!(client_version_line("0.13.1"), "bundled client: lettuce-volunteer 0.13.1");
    }

    #[test]
    fn web_view_levels_map_and_unknown_ones_are_info() {
        assert_eq!(webview_level("error"), Level::Error);
        assert_eq!(webview_level("warn"), Level::Warn);
        assert_eq!(webview_level("debug"), Level::Debug);
        assert_eq!(webview_level("info"), Level::Info);
        assert_eq!(webview_level("shout"), Level::Info);
    }

    #[test]
    fn timestamps_carry_their_offset() {
        let ts = timestamp();
        assert!(
            ts.ends_with('Z') || ts[ts.len() - 6..].starts_with(['+', '-']),
            "{ts}"
        );
    }
}
