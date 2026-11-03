param(
    [string]$Version = "1.0.3"
)

$ErrorActionPreference = "Stop"
$root = $PSScriptRoot
$dist = Join-Path $root "dist"
$stage = Join-Path $dist (Join-Path "stage-v$Version" "CodexModelRouter")
New-Item -ItemType Directory -Path $stage -Force | Out-Null

$targets = @(
    @{ GOOS = "windows"; GOARCH = "amd64"; Output = "bin/windows-amd64/codex-router.exe" },
    @{ GOOS = "windows"; GOARCH = "arm64"; Output = "bin/windows-arm64/codex-router.exe" },
    @{ GOOS = "darwin"; GOARCH = "amd64"; Output = "bin/darwin-amd64/codex-router" },
    @{ GOOS = "darwin"; GOARCH = "arm64"; Output = "bin/darwin-arm64/codex-router" }
)

foreach ($target in $targets) {
    $output = Join-Path $stage $target.Output
    New-Item -ItemType Directory -Path (Split-Path -Parent $output) -Force | Out-Null
    $oldGOOS = $env:GOOS
    $oldGOARCH = $env:GOARCH
    try {
        $env:GOOS = $target.GOOS
        $env:GOARCH = $target.GOARCH
        $env:CGO_ENABLED = "0"
        go build -trimpath -ldflags "-s -w" -o $output .
        if ($LASTEXITCODE -ne 0) { throw "Build failed for $($target.GOOS)/$($target.GOARCH)" }
    }
    finally {
        $env:GOOS = $oldGOOS
        $env:GOARCH = $oldGOARCH
        Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue
    }
}

$releaseFiles = @(
    "README.md", "install-windows.ps1", "uninstall-windows.ps1",
    "doctor-windows.ps1", "install-windows.cmd", "install-macos.sh",
    "uninstall-macos.sh", "doctor-macos.sh", "Install-Codex-Model-Router.command",
    "change-deepseek-key-windows.ps1", "change-deepseek-key-windows.cmd",
    "change-deepseek-key-macos.sh", "Change-DeepSeek-API-Key.command"
)
foreach ($name in $releaseFiles) {
    Copy-Item -LiteralPath (Join-Path $root $name) -Destination (Join-Path $stage $name)
}
Set-Content -LiteralPath (Join-Path $stage "VERSION") -Value $Version -Encoding ASCII

$zip = Join-Path $dist "CodexModelRouter-v$Version.zip"
Compress-Archive -LiteralPath $stage -DestinationPath $zip -CompressionLevel Optimal -Force
$tarball = Join-Path $dist "CodexModelRouter-v$Version-macOS.tar.gz"
$python = Get-Command python -ErrorAction SilentlyContinue
if ($null -eq $python) { throw "Python is required only to package the macOS tar archive." }
& $python.Source (Join-Path $root "package_macos.py") $stage $tarball
if ($LASTEXITCODE -ne 0) { throw "macOS package creation failed" }
$hash = Get-FileHash -LiteralPath $zip -Algorithm SHA256
"$($hash.Hash.ToLowerInvariant())  $([IO.Path]::GetFileName($zip))" |
    Set-Content -LiteralPath "$zip.sha256" -Encoding ASCII
$tarHash = Get-FileHash -LiteralPath $tarball -Algorithm SHA256
"$($tarHash.Hash.ToLowerInvariant())  $([IO.Path]::GetFileName($tarball))" |
    Set-Content -LiteralPath "$tarball.sha256" -Encoding ASCII
$hash
$tarHash
