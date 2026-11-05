#Requires -Version 5.1
param(
    [string]$CodexHome = "",
    [ValidateRange(1024, 65535)]
    [int]$Port = 49627,
    [switch]$NoService
)

$ErrorActionPreference = "Stop"
$architecture = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
$binaryName = switch ($architecture) {
    "X64" { "windows-amd64\codex-router.exe" }
    "Arm64" { "windows-arm64\codex-router.exe" }
    default { throw "Unsupported Windows architecture: $architecture" }
}
$binary = Join-Path $PSScriptRoot (Join-Path "bin" $binaryName)
if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) {
    throw "Missing router binary: $binary"
}

if ([string]::IsNullOrWhiteSpace($CodexHome)) {
    $CodexHome = Join-Path $HOME ".codex"
}
$dataDir = Join-Path $CodexHome "codex-model-router"
$keyFile = Join-Path $dataDir "deepseek.key"
$temporaryEnvironmentKey = $false

if ([string]::IsNullOrWhiteSpace($env:DEEPSEEK_API_KEY) -and -not (Test-Path -LiteralPath $keyFile)) {
    Write-Host "First-time setup: enter the DeepSeek API key. Input is hidden."
    $secureKey = Read-Host -AsSecureString "DeepSeek API Key (press Enter to reuse config.toml)"
    if ($null -ne $secureKey -and $secureKey.Length -gt 0) {
        $pointer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secureKey)
        try {
            $env:DEEPSEEK_API_KEY = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($pointer)
            $temporaryEnvironmentKey = $true
        }
        finally {
            [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($pointer)
        }
    }
}

$arguments = @("install", "--codex-home", $CodexHome, "--port", $Port)
if ($NoService) { $arguments += "--no-service" }
try {
    & $binary @arguments
    if ($LASTEXITCODE -ne 0) { throw "Installation failed with exit code $LASTEXITCODE" }
    if (-not $NoService) {
        & $binary status --codex-home $CodexHome
        if ($LASTEXITCODE -ne 0) { throw "Health check failed with exit code $LASTEXITCODE" }
    }
}
finally {
    if ($temporaryEnvironmentKey) {
        Remove-Item Env:DEEPSEEK_API_KEY -ErrorAction SilentlyContinue
    }
}

Write-Host ""
Write-Host "Installed. Finish active Codex work and restart the desktop app."
Write-Host "The native model menu will show OpenAI and DeepSeek models together."
