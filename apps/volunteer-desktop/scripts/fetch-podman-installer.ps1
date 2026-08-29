<#
.SYNOPSIS
Downloads the Podman for Windows installer that the desktop app bundles, if it is not already
present and verified.

.DESCRIPTION
Leaves (computations) that run as containers need a container runtime on the volunteer's
machine. On Windows the desktop app can install one for the volunteer: it ships the official
Podman installer inside its own installer and runs it silently, per-user, when the volunteer
opts in (see src-tauri/src/podman_installer.rs). The Windows bundle configuration
(src-tauri/tauri.windows.conf.json) therefore lists

    src-tauri/resources/podman-installer-windows-amd64.msi

as a bundled resource. That file is 27 MB and is not committed; this script fetches the exact
release the app was tested with from the Podman project's GitHub releases and checks its size
and SHA-256 digest before putting it in place. The digest comes from the "shasums" asset
attached to the same Podman release.

Run this before `npm run tauri build` (or `cargo check`/`cargo build` in src-tauri) on Windows.
It is a no-op when the file is already present and its digest matches.

To move to a newer Podman: update $PodmanVersion, $ExpectedSize and $ExpectedSha256 below from
the new release's asset list and shasums file, run the script with -Force, re-test the in-app
installation flow, and update the version noted in the app README.

.PARAMETER Force
Download again even if a verified copy is already present.

.EXAMPLE
pwsh scripts/fetch-podman-installer.ps1
#>
[CmdletBinding()]
param(
    [switch]$Force
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$ProgressPreference = 'SilentlyContinue'

$PodmanVersion  = '5.8.1'
$ExpectedSize   = 27324416
$ExpectedSha256 = '1ddee378080ce727023d58ff594b692b7174c85fc2e097a0a2250b6347483cbd'

$asset   = 'podman-installer-windows-amd64.msi'
$url     = "https://github.com/containers/podman/releases/download/v$PodmanVersion/$asset"
$appDir  = Split-Path -Parent $PSScriptRoot
$destDir = Join-Path $appDir 'src-tauri\resources'
$dest    = Join-Path $destDir $asset

function Test-Installer([string]$path) {
    if (-not (Test-Path $path)) { return $false }
    if ((Get-Item $path).Length -ne $ExpectedSize) { return $false }
    return (Get-FileHash $path -Algorithm SHA256).Hash.ToLowerInvariant() -eq $ExpectedSha256
}

if (-not $Force -and (Test-Installer $dest)) {
    Write-Host "Podman $PodmanVersion installer already present and verified: $dest"
    exit 0
}

New-Item -ItemType Directory -Force $destDir | Out-Null
$tmp = "$dest.download"
Write-Host "Downloading Podman $PodmanVersion installer from $url"
Invoke-WebRequest -Uri $url -OutFile $tmp -UseBasicParsing

$size = (Get-Item $tmp).Length
$hash = (Get-FileHash $tmp -Algorithm SHA256).Hash.ToLowerInvariant()
if ($size -ne $ExpectedSize -or $hash -ne $ExpectedSha256) {
    Remove-Item $tmp -Force
    throw "Downloaded installer does not match the pinned release (size $size, sha256 $hash; expected $ExpectedSize, $ExpectedSha256)"
}
Move-Item $tmp $dest -Force
Write-Host "Verified Podman $PodmanVersion installer (sha256 $hash): $dest"
