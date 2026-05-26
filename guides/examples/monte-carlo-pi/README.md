# Monte Carlo Pi — native example

A tiny native compute binary that estimates π by throwing random darts at a unit
square. Each work unit runs one trial (1,000,000 darts) with a unique seed; the head's
Monte Carlo aggregator averages the trials, converging on π ≈ 3.14159.

- **Language:** Go (single file, no dependencies)
- **Task pattern:** `MONTE_CARLO`
- **Compute contract:** reads `LETTUCE_PARAMS_FILE`, writes `LETTUCE_OUTPUT_FILE`
  (the binary also understands the container variables, so the same code runs either way)

## Build

```bash
GOOS=linux   GOARCH=amd64 go build -o monte-carlo-pi-linux .
GOOS=windows GOARCH=amd64 go build -o monte-carlo-pi.exe   .
```

## Compute checksums (required for NATIVE leafs)

Volunteers verify each native binary against a SHA-256 checksum before running it,
so you must supply one per platform in the leaf's `binary_checksums`:

```bash
# Linux
sha256sum monte-carlo-pi-linux monte-carlo-pi.exe

# macOS
shasum -a 256 monte-carlo-pi-linux monte-carlo-pi.exe

# Windows PowerShell — lowercase the hash before pasting
Get-FileHash -Algorithm SHA256 monte-carlo-pi-linux | ForEach-Object { $_.Hash.ToLower() }
Get-FileHash -Algorithm SHA256 monte-carlo-pi.exe   | ForEach-Object { $_.Hash.ToLower() }
```

Each digest is a 64-character lowercase hex string; paste it into
`execution_config.binary_checksums` under the matching platform key (see
first-leaf.md Step 5).

## Test locally

```bash
echo '{"seed": 42}' > params.json
LETTUCE_PARAMS_FILE=params.json LETTUCE_OUTPUT_FILE=out.json ./monte-carlo-pi-linux
cat out.json     # {"result": 3.14..., "seed": 42, ...}
```

## Deploy it to your head

This is the native walkthrough in **[../../first-leaf.md](../../first-leaf.md)** —
build → host on your head → create, configure, activate the leaf → generate work units →
aggregate.
