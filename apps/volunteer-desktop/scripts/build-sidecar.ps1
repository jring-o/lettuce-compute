<#
.SYNOPSIS
Builds (or downloads) the lettuce-volunteer sidecar binary that the desktop app bundles.

.DESCRIPTION
The desktop app ("Lettuce Compute") is a shell around the volunteer command-line client,
lettuce-volunteer. The app starts that client as a background daemon and talks to it over the
daemon's localhost management API. Tauri bundles the client as an "external binary" (a sidecar)
and expects to find it at

    src-tauri/binaries/lettuce-volunteer-<rust target triple>[.exe]

for example src-tauri/binaries/lettuce-volunteer-x86_64-pc-windows-msvc.exe.

By default this script compiles the client from this repository's services/volunteer-cli source
with the same flags the CLI release workflow uses (GOWORK=off, CGO_ENABLED=0, -trimpath) and
stamps it with the version `git describe --tags` reports for the checkout, leading "v" removed,
which is how the release workflow derives the version from a release tag. With -FromRelease it
instead downloads the already-published client from a GitHub release of this repository and
verifies the download against the release's .sha256 checksum file.

.PARAMETER Target
Rust target triple to produce the sidecar for. Defaults to the host triple reported by
`rustc -vV`. Supported: x86_64-pc-windows-msvc, aarch64-pc-windows-msvc, x86_64-apple-darwin,
aarch64-apple-darwin, x86_64-unknown-linux-gnu, aarch64-unknown-linux-gnu.

.PARAMETER FromRelease
A CLI release tag of this repository, for example v0.11.1. Download that release's
lettuce-volunteer asset for the target instead of compiling from source.

.PARAMETER Version
Override the version string stamped into a compiled binary. Ignored with -FromRelease.

.EXAMPLE
pwsh scripts/build-sidecar.ps1
Compile for this machine.

.EXAMPLE
pwsh scripts/build-sidecar.ps1 -Target aarch64-pc-windows-msvc
Cross-compile for Windows on ARM.

.EXAMPLE
pwsh scripts/build-sidecar.ps1 -FromRelease v0.11.1
Use the client published with CLI release v0.11.1 instead of building one.
#>
[CmdletBinding()]
param(
    [string]$Target,
    [string]$FromRelease,
    [string]$Version
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
# Windows PowerShell 5.1 renders a progress bar for every download chunk, which makes
# Invoke-WebRequest dramatically slower. Silence it.
$ProgressPreference = 'SilentlyContinue'

$appDir      = Split-Path -Parent $PSScriptRoot
$repoDir     = (Resolve-Path (Join-Path $appDir '..\..')).Path
$cliDir      = Join-Path $repoDir 'services\volunteer-cli'
$outDir      = Join-Path $appDir 'src-tauri\binaries'
$releaseBase = 'https://github.com/jring-o/lettuce-compute/releases/download'
$ldflagsPath = 'github.com/lettuce-compute/volunteer-cli/internal/cli.version'

# Rust target triple -> the Go platform the CLI release workflow publishes for it.
$platforms = @{
    'x86_64-pc-windows-msvc'    = @{ GOOS = 'windows'; GOARCH = 'amd64'; Ext = '.exe' }
    'aarch64-pc-windows-msvc'   = @{ GOOS = 'windows'; GOARCH = 'arm64'; Ext = '.exe' }
    'x86_64-apple-darwin'       = @{ GOOS = 'darwin';  GOARCH = 'amd64'; Ext = '' }
    'aarch64-apple-darwin'      = @{ GOOS = 'darwin';  GOARCH = 'arm64'; Ext = '' }
    'x86_64-unknown-linux-gnu'  = @{ GOOS = 'linux';   GOARCH = 'amd64'; Ext = '' }
    'aarch64-unknown-linux-gnu' = @{ GOOS = 'linux';   GOARCH = 'arm64'; Ext = '' }
}

function Get-HostTriple {
    if (Get-Command rustc -ErrorAction SilentlyContinue) {
        foreach ($line in (& rustc -vV)) {
            if ($line -match '^host:\s*(\S+)') { return $Matches[1] }
        }
    }
    # No Rust toolchain on PATH: infer the Windows triple from the processor architecture.
    if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { return 'aarch64-pc-windows-msvc' }
    return 'x86_64-pc-windows-msvc'
}

function Get-SourceVersion {
    # Same derivation as the CLI release workflow (tag "v1.2.3" stamps version "1.2.3");
    # between releases this yields e.g. "0.11.1-3-g49d0ac7". Only CLI-style "v*" tags are
    # considered, so desktop release tags (desktop-v*) never leak into the client's version.
    $describe = & git -C $repoDir describe --tags --match 'v[0-9]*' --always
    if ($LASTEXITCODE -ne 0 -or -not $describe) {
        throw "git describe failed in $repoDir (is this a git checkout with history?)"
    }
    return ($describe.Trim() -replace '^v', '')
}

function Assert-ExitCode([string]$what) {
    if ($LASTEXITCODE -ne 0) { throw "$what failed with exit code $LASTEXITCODE" }
}

if (-not $Target) { $Target = Get-HostTriple }
if (-not $platforms.ContainsKey($Target)) {
    throw "Unsupported target triple '$Target'. Supported: $($platforms.Keys -join ', ')"
}
$platform = $platforms[$Target]
$outPath  = Join-Path $outDir "lettuce-volunteer-$Target$($platform.Ext)"
New-Item -ItemType Directory -Force $outDir | Out-Null

if ($FromRelease) {
    $asset   = "lettuce-volunteer-$($platform.GOOS)-$($platform.GOARCH)$($platform.Ext)"
    $url     = "$releaseBase/$FromRelease/$asset"
    $tmpFile = "$outPath.download"

    Write-Host "Downloading $url"
    Invoke-WebRequest -Uri $url -OutFile $tmpFile -UseBasicParsing
    Invoke-WebRequest -Uri "$url.sha256" -OutFile "$tmpFile.sha256" -UseBasicParsing

    # The checksum file is `sha256sum` output: "<hex digest>  <file name>".
    $expected = ((Get-Content "$tmpFile.sha256" -Raw).Trim() -split '\s+')[0].ToLowerInvariant()
    $actual   = (Get-FileHash $tmpFile -Algorithm SHA256).Hash.ToLowerInvariant()
    Remove-Item "$tmpFile.sha256" -Force
    if ($expected -notmatch '^[0-9a-f]{64}$' -or $expected -ne $actual) {
        Remove-Item $tmpFile -Force
        throw "SHA-256 mismatch for $asset from $FromRelease (expected $expected, got $actual)"
    }
    Move-Item $tmpFile $outPath -Force
    Write-Host "Verified SHA-256 $actual"
} else {
    if (-not $Version) { $Version = Get-SourceVersion }
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        throw 'Go is not on PATH; install it (see services/volunteer-cli/go.mod for the version) or use -FromRelease.'
    }

    Write-Host "Building lettuce-volunteer $Version for $Target ($($platform.GOOS)/$($platform.GOARCH))"
    $saved = @{}
    foreach ($name in 'GOWORK', 'CGO_ENABLED', 'GOOS', 'GOARCH') {
        $saved[$name] = [Environment]::GetEnvironmentVariable($name)
    }
    Push-Location $cliDir
    try {
        # GOWORK=off: the repo-root go.work lists only the head services, so a standalone build
        # of the client must ignore it. CGO_ENABLED=0 matches the release workflow and keeps the
        # binary self-contained, which is what makes the cross-compile a plain `go build`.
        $env:GOWORK      = 'off'
        $env:CGO_ENABLED = '0'
        $env:GOOS        = $platform.GOOS
        $env:GOARCH      = $platform.GOARCH
        & go build -trimpath -ldflags "-X $ldflagsPath=$Version" -o $outPath ./cmd/lettuce-volunteer/
        Assert-ExitCode 'go build'
    } finally {
        Pop-Location
        foreach ($name in $saved.Keys) {
            [Environment]::SetEnvironmentVariable($name, $saved[$name])
        }
    }
}

Write-Host "Sidecar ready: $outPath"
if ($Target -eq (Get-HostTriple)) {
    # Same platform as this machine, so the binary can be executed here: prove the stamp.
    & $outPath --version
    Assert-ExitCode "$outPath --version"
}
