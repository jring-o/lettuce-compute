use std::fs;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

/// Shared state for the current viz bundle directory.
pub struct VizBaseDir(pub Arc<Mutex<Option<String>>>);

/// Set the current viz bundle directory for the custom protocol handler.
///
/// `path` is the daemon's `viz_bundle_path`: the directory holding the
/// bundle's `index.html`. The daemon already resolves a tarball that wraps
/// everything in one top-level folder (`runtime.resolveVizRoot`), and the same
/// tolerance is applied here so a path to the unresolved extraction directory
/// still works. Fails with a readable message when the directory is gone
/// (the unit's work directory, which holds the bundle, is removed once the
/// unit completes) or holds no `index.html`.
#[tauri::command]
pub fn set_viz_base(state: tauri::State<'_, VizBaseDir>, path: String) -> Result<(), String> {
    let root = resolve_bundle_root(Path::new(&path))?;
    *state.0.lock().unwrap() = Some(root.to_string_lossy().to_string());
    Ok(())
}

/// Find the directory to serve for a bundle path: the path itself when it
/// holds `index.html`, else its single subdirectory when that one does.
fn resolve_bundle_root(path: &Path) -> Result<PathBuf, String> {
    let canon = path
        .canonicalize()
        .map_err(|e| format!("visualization bundle not found at {}: {e}", path.display()))?;
    if canon.join("index.html").is_file() {
        return Ok(canon);
    }

    let entries = fs::read_dir(&canon)
        .map_err(|e| format!("read visualization bundle {}: {e}", canon.display()))?;
    let mut sub_dirs: Vec<PathBuf> = entries
        .filter_map(|e| e.ok())
        .map(|e| e.path())
        .filter(|p| p.is_dir())
        .collect();
    if sub_dirs.len() == 1 {
        let nested = sub_dirs.remove(0);
        if nested.join("index.html").is_file() {
            return Ok(nested);
        }
    }

    Err(format!(
        "visualization bundle at {} has no index.html",
        canon.display()
    ))
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

#[cfg(test)]
mod tests {
    use super::*;

    /// A fresh, empty directory under the system temp dir.
    fn temp_dir(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "lettuce-viz-test-{tag}-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        fs::create_dir_all(&dir).unwrap();
        dir
    }

    #[test]
    fn bundle_root_is_the_directory_holding_index_html() {
        let dir = temp_dir("root");
        fs::write(dir.join("index.html"), "<html></html>").unwrap();
        let root = resolve_bundle_root(&dir).unwrap();
        assert_eq!(root, dir.canonicalize().unwrap());
        fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn bundle_root_tolerates_a_single_wrapper_directory() {
        let dir = temp_dir("wrapper");
        let nested = dir.join("bundle");
        fs::create_dir_all(&nested).unwrap();
        fs::write(nested.join("index.html"), "<html></html>").unwrap();
        let root = resolve_bundle_root(&dir).unwrap();
        assert_eq!(root, nested.canonicalize().unwrap());
        fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn bundle_root_rejects_a_directory_without_index_html() {
        let dir = temp_dir("noindex");
        fs::write(dir.join("main.js"), "").unwrap();
        let err = resolve_bundle_root(&dir).unwrap_err();
        assert!(err.contains("no index.html"), "{err}");
        fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn bundle_root_reports_a_missing_directory() {
        let dir = temp_dir("missing").join("gone");
        let err = resolve_bundle_root(&dir).unwrap_err();
        assert!(err.contains("not found"), "{err}");
    }

    #[test]
    fn read_file_rejects_traversal_and_absolute_paths() {
        let dir = temp_dir("traversal");
        fs::write(dir.join("data.json"), "{}").unwrap();
        let work = dir.to_string_lossy().to_string();
        assert!(viz_read_file(work.clone(), "../secret".into()).is_err());
        assert!(viz_read_file(work.clone(), "/etc/passwd".into()).is_err());
        assert_eq!(viz_read_file(work, "data.json".into()).unwrap(), b"{}");
        fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn list_files_returns_relative_paths_filtered_by_glob() {
        let dir = temp_dir("list");
        fs::create_dir_all(dir.join("frames")).unwrap();
        fs::write(dir.join("state.json"), "{}").unwrap();
        fs::write(dir.join("frames").join("0001.bin"), "").unwrap();
        let work = dir.to_string_lossy().to_string();
        let mut all = viz_list_files(work.clone(), None).unwrap();
        all.sort();
        assert_eq!(all, vec!["frames/0001.bin", "state.json"]);
        let json = viz_list_files(work, Some("*.json".into())).unwrap();
        assert_eq!(json, vec!["state.json"]);
        fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn simple_glob_matches_suffix_prefix_and_exact() {
        assert!(matches_simple_glob("frames/0001.bin", "*.bin"));
        assert!(matches_simple_glob("frames/0001.bin", "0001*"));
        assert!(matches_simple_glob("frames/0001.bin", "0001.bin"));
        assert!(!matches_simple_glob("frames/0001.bin", "*.json"));
    }
}
