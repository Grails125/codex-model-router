#Requires -Version 5.1
param([string]$CodexHome = "")

$ErrorActionPreference = "Stop"
$architecture = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
$binaryName = switch ($architecture) {
    "X64" { "windows-amd64\codex-router.exe" }
    "Arm64" { "windows-arm64\codex-router.exe" }
    default { throw "Unsupported Windows architecture: $architecture" }
}
$binary = Join-Path $PSScriptRoot (Join-Path "bin" $binaryName)
if ([string]::IsNullOrWhiteSpace($CodexHome)) {
    $CodexHome = Join-Path $HOME ".codex"
}
& $binary doctor --codex-home $CodexHome
exit $LASTEXITCODE
