use std::fs;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

/// Shared state for the current viz bundle directory.
pub struct VizBaseDir(pub Arc<Mutex<Option<String>>>);

/// Set the current viz bundle directory for the custom protocol handler.
#[tauri::command]
pub fn set_viz_base(state: tauri::State<'_, VizBaseDir>, path: String) -> Result<(), String> {
    let canon = Path::new(&path)
        .canonicalize()
        .map_err(|e| format!("invalid viz path: {e}"))?;
    *state.0.lock().unwrap() = Some(canon.to_string_lossy().to_string());
    Ok(())
}

/// Validate that `target` is safely within `base_dir` (no path traversal).
/// Returns the canonicalized target path on success.
fn validate_within(base_dir: &Path, rel_path: &str) -> Result<PathBuf, String> {
    // Reject obviously malicious paths early.
    if rel_path.contains("..") || rel_path.starts_with('/') || rel_path.starts_with('\\') {
        return Err(format!("invalid path: {}", rel_path));
    }

    let base = base_dir
        .canonicalize()
        .map_err(|e| format!("canonicalize base: {}", e))?;

    let target = base.join(rel_path);
    let target = target
        .canonicalize()
        .map_err(|e| format!("canonicalize target: {}", e))?;

    if !target.starts_with(&base) {
        return Err(format!("path escape detected: {}", rel_path));
    }

    Ok(target)
}

/// Read a file from within a work directory. `rel_path` must be relative
/// and stay within `work_dir` (path traversal is rejected).
#[tauri::command]
pub fn viz_read_file(work_dir: String, rel_path: String) -> Result<Vec<u8>, String> {
    let base = Path::new(&work_dir);
    let target = validate_within(base, &rel_path)?;
    fs::read(&target).map_err(|e| format!("read file: {}", e))
}

/// List files in a work directory, optionally filtered by a simple glob pattern.
/// Returns paths relative to `work_dir`. All paths are validated to stay within
/// the work directory.
#[tauri::command]
pub fn viz_list_files(work_dir: String, pattern: Option<String>) -> Result<Vec<String>, String> {
    let base = Path::new(&work_dir);
    let base_canon = base
        .canonicalize()
        .map_err(|e| format!("canonicalize base: {}", e))?;

    let mut results = Vec::new();

    if let Some(pat) = pattern {
        // Simple glob: only support *.ext patterns by filtering recursively.
        collect_files_recursive(&base_canon, &base_canon, &Some(pat), &mut results)?;
    } else {
        collect_files_recursive(&base_canon, &base_canon, &None, &mut results)?;
    }

    Ok(results)
}

/// Recursively collect files under `dir`, returning paths relative to `base`.
fn collect_files_recursive(
    base: &Path,
    dir: &Path,
    pattern: &Option<String>,
    results: &mut Vec<String>,
) -> Result<(), String> {
    let entries = fs::read_dir(dir).map_err(|e| format!("read dir: {}", e))?;

    for entry in entries {
        let entry = entry.map_err(|e| format!("read entry: {}", e))?;
        let path = entry.path();

        // Verify this path is within the base (protects against symlink escapes).
        if let Ok(canonical) = path.canonicalize() {
            if !canonical.starts_with(base) {
                continue; // skip symlinks that escape
            }
        }

        if path.is_dir() {
            collect_files_recursive(base, &path, pattern, results)?;
        } else if path.is_file() {
            let rel = path
                .strip_prefix(base)
                .map_err(|e| format!("strip prefix: {}", e))?;
            let rel_str = rel.to_string_lossy().replace('\\', "/");

            if let Some(pat) = pattern {
                if matches_simple_glob(&rel_str, pat) {
                    results.push(rel_str);
                }
            } else {
                results.push(rel_str);
            }
        }
    }

    Ok(())
}

/// Simple glob matching: supports `*.ext` and `prefix*` patterns.
fn matches_simple_glob(filename: &str, pattern: &str) -> bool {
    if let Some(suffix) = pattern.strip_prefix('*') {
        // *.ext — match by suffix (check against filename component)
        let name = filename.rsplit('/').next().unwrap_or(filename);
        name.ends_with(suffix)
    } else if let Some(prefix) = pattern.strip_suffix('*') {
        let name = filename.rsplit('/').next().unwrap_or(filename);
        name.starts_with(prefix)
    } else {
        // Exact match against filename component
        let name = filename.rsplit('/').next().unwrap_or(filename);
        name == pattern
    }
}
