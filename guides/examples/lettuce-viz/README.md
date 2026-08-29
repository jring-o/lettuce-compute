# Visualization bundles

A **leaf** (a computation hosted on a Lettuce **head**) can ship a small web page that
draws what its **work units** are doing. Lettuce shows that page in two places:

- **Live**, in the volunteer desktop app, while a work unit runs on the volunteer's
  machine. The page can read the files the computation writes and redraw as they change.
- **Replay**, after a work unit has finished, from the result it produced. The head's
  dashboard replays results for the leaf's owner (or for everyone, if the leaf opts in),
  and the desktop app replays results kept on the volunteer's own machine.

The page is called a **visualization bundle** (or "viz bundle"). This directory holds
`lettuce-viz.js`, an optional helper that wraps the messaging protocol described below.
The protocol is plain `window.postMessage`; the helper is a convenience, not a requirement.

## What a bundle is

A `.tar.gz` archive with an `index.html` at its root. Everything the page needs (scripts,
styles, fonts, images, WebAssembly) must be inside the archive and referenced by relative
path — the page runs in a sandboxed frame with no network access to the outside world.

```
viz.tar.gz
├── index.html
├── main.js
└── style.css
```

The archive may also wrap everything in one top-level folder (`bundle/index.html`, the
shape `tar -czf viz.tar.gz bundle/` produces). Both the head and the volunteer client
accept either layout.

Build it from a directory:

```bash
tar -C my-viz -czf viz.tar.gz .
sha256sum viz.tar.gz            # keep this for the checksum below
```

Limits enforced when the archive is unpacked: 500 MB of uncompressed content in total,
100 MB per file, and no entry may point outside the archive root.

## Attaching a bundle to a leaf

Host the archive somewhere the head allows artifacts to be served from (for most heads,
the head's own `binaries/` directory — see `guides/first-leaf.md`), then reference it in the
leaf's `execution_config`:

```json
"execution_config": {
  "image": "your-domain.com/nbody:latest",
  "binaries": {
    "viz": "https://your-domain.com/binaries/nbody-viz.tar.gz"
  },
  "binary_checksums": {
    "viz": "3f0c…e91a"
  }
}
```

The `viz` key of `binaries` is the bundle's download URL. It works with any runtime
(container, native, or WebAssembly) and sits alongside the runtime's own entries
(`image`, or the per-platform binary URLs). The matching `binary_checksums.viz` entry — the
SHA-256 of the archive as lowercase hex — is optional but recommended: with it, the
volunteer client caches the bundle by content and refuses a download whose digest does not
match; without it, the bundle is fetched unverified (the client logs a warning).

The bundle is downloaded once per volunteer machine and cached under
`~/.lettuce/cache/viz/`. Change the URL (or the checksum) when you publish a new version.

## How the bundle is shown

The bundle's `index.html` is loaded in an `<iframe sandbox="allow-scripts allow-same-origin">`
on an origin of its own — never the app's or the dashboard's origin — so it holds no
credentials and cannot reach either host's API. Its only channel to the outside is
`window.postMessage` with the embedding page (the **host**). The host sends the page its
mode and data; the page asks the host for files. Every message is a plain object with a
`type` field.

### Handshake

1. The page posts `{ type: "vizReady" }` to `window.parent` once it is ready to receive
   messages. Post it more than once if you like — the host ignores repeats. The helper
   re-posts it every 200 ms, up to five times, until initialised, which covers a page that
   loads before the host's listener is attached.
2. The host answers with `vizInit`:

   ```json
   {
     "type": "vizInit",
     "mode": "live",
     "workDir": "/home/alice/.lettuce/container-work/8c2d…",
     "leafSlug": "nbody-gravity",
     "params": {}
   }
   ```

   | Field      | Meaning |
   |------------|---------|
   | `mode`     | `"live"` while the unit runs on this machine; `"replay"` when showing a finished result. |
   | `workDir`  | Live mode: the absolute path of the unit's work directory on the volunteer's machine, for display only — file requests use paths relative to it. Empty string in replay. |
   | `leafSlug` | The leaf's URL slug. In the desktop app's live view this is the leaf's **display name** (the running daemon reports no slug for an active unit); the dashboard and both replay paths send the real slug. Do not key logic on it. |
   | `params`   | Reserved; always `{}` today. |

If the host never sees `vizReady` (for instance the page was served from cache and its
script ran before the host listener existed) the desktop app sends `vizInit` anyway about
150 ms after the frame's `load` event.

### Live mode: reading the work directory

In live mode the page can read files the computation writes to the unit's **work
directory** — the per-unit folder the volunteer client runs the unit in (its
`output/` folder is where a container leaf's `LETTUCE_OUTPUT_DIR` points; a native leaf
writes wherever its contract says, under the same folder). Paths are always **relative to
the work directory**; an absolute path, a path containing `..`, or a path with an empty
segment is refused, and symbolic links that leave the directory are ignored.

| Page → host | Host → page | Notes |
|---|---|---|
| `{ type: "readFile", id, path }` | `{ type: "readFileResult", id, data, error? }` | `data` is the file's bytes as an array of numbers (the helper converts it to an `ArrayBuffer`); on failure `data` is `null` and `error` is a message. |
| `{ type: "listFiles", id, pattern? }` | `{ type: "listFilesResult", id, files, error? }` | `files` is an array of relative paths (forward slashes), recursive. `pattern` matches the file name only: `*.json`, `frame-*`, or an exact name. |
| `{ type: "watchFile", path, interval? }` | `{ type: "fileChanged", path, data }` | The host polls the file every `interval` ms (default 1000) and posts `fileChanged` each time its content changes, including the first time it appears. |
| `{ type: "unwatchFile", path }` | — | Stops the poll. |

`id` is any string unique to the request; the host echoes it so the page can match
replies. The desktop app drops every watch when the unit is paused (and when the frame
closes) and does not restore them on resume, without telling the page — a page that must
keep following a file across a pause should re-issue `watchFile` when its data stops
arriving, or poll with `readFile` on its own timer.

In **replay** mode these four requests are ignored (no reply is sent): there is no work
directory any more. A page that supports both modes should branch on `vizInit.mode`.

### Replay mode: the result

After `vizInit` with `mode: "replay"` the host posts one `replayData` message. It is the
work unit's result — the JSON object your computation wrote as its output — spread into
the message alongside `type`:

```json
{ "type": "replayData", "keyframe_snapshots": [...], "time_series": {...} }
```

So a leaf whose output is `{"energy": [...], "positions": [...]}` receives
`{type: "replayData", energy: [...], positions: [...]}`. Design your output with replay in
mind: it is what the page has to draw from, and it is stored in full on the head. The
desktop app delays this message about 50 ms after `vizInit` so the page can finish
initialising.

**On the head's dashboard**, `/leafs/<slug>/visualize` lists the leaf's completed work
units and replays the selected one. Results are the leaf's content, so the page is visible
to the leaf's owner and head administrators only, unless the leaf's `results_visibility`
is set to `PUBLIC` (see `guides/first-leaf.md`). The dashboard fetches and unpacks the
bundle itself and serves it from a dedicated visualization origin (`VIZ_ORIGIN`).

**In the desktop app**, the volunteer client keeps a copy of each result it produced for a
leaf that has a bundle (under `~/.lettuce/results/`, oldest evicted first when the cache
exceeds its size limit). The History page offers **View Visualization** for those units
and replays them in the same frame used for live view.

## Minimal example

`index.html`:

```html
<!doctype html>
<meta charset="utf-8">
<style>html,body{margin:0;background:#0a0a0f;color:#e4e4e7;font:14px system-ui}</style>
<pre id="out">waiting for host…</pre>
<script type="module">
  import { createVizClient } from "./lettuce-viz.js";

  const out = document.getElementById("out");
  const viz = createVizClient();
  const init = await viz.ready();          // posts vizReady, resolves on vizInit

  if (init.mode === "live") {
    // Redraw whenever the computation rewrites its progress file.
    viz.watchFile("output/progress.json", 1000, (buf) => {
      const progress = JSON.parse(new TextDecoder().decode(buf));
      out.textContent = `step ${progress.step} of ${progress.total}`;
    });
  } else {
    // Replay: the finished result arrives once.
    viz.onReplayData((result) => {
      out.textContent = JSON.stringify(result, null, 2);
    });
  }
</script>
```

Copy `lettuce-viz.js` from this directory next to `index.html`, then
`tar -C my-viz -czf viz.tar.gz .`.

## The helper: `lettuce-viz.js`

`createVizClient()` returns an object whose methods map one-to-one onto the protocol:

| Method | Does |
|---|---|
| `ready()` | Posts `vizReady` (with retries) and resolves with `{ mode, workDir, leafSlug, params }` from `vizInit`. Resolves immediately if `vizInit` already arrived. |
| `readFile(path)` | Resolves with an `ArrayBuffer`; rejects with the host's error message. |
| `listFiles(pattern?)` | Resolves with an array of relative paths (empty if the host sent none). |
| `watchFile(path, intervalMs, callback)` | Calls `callback(ArrayBuffer)` on each `fileChanged` for that path. |
| `unwatchFile(path)` | Stops the watch and drops the callback. |
| `onReplayData(callback)` | Calls `callback(message)` with the whole `replayData` message (including `type`). |

The helper registers a single `message` listener when created; create one client per page.
It is an ES module with no dependencies, and the desktop app's test-suite exercises this
exact file (`apps/volunteer-desktop/src/__tests__/lettuce-viz-sdk.test.ts`).

## Checklist before you publish

- `index.html` is at the archive root (or inside a single top-level folder).
- Every asset is inside the archive and referenced relatively; nothing is fetched from the
  network.
- The page handles both `mode: "live"` and `mode: "replay"`, or clearly does nothing in the
  one it does not support.
- Live-mode file paths are relative to the work directory and match what the computation
  actually writes.
- `binary_checksums.viz` is the SHA-256 of the exact archive you uploaded.
