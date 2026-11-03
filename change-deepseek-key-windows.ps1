#Requires -Version 5.1
param(
    [string]$CodexHome = ""
)

$ErrorActionPreference = "Stop"
if ([string]::IsNullOrWhiteSpace($CodexHome)) {
    $CodexHome = Join-Path $HOME ".codex"
}
$architecture = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
$platform = switch ($architecture) {
    "X64" { "windows-amd64" }
    "Arm64" { "windows-arm64" }
    default { throw "Unsupported Windows architecture: $architecture" }
}
$binary = Join-Path $PSScriptRoot "bin\$platform\codex-router.exe"
if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) {
    $binary = Join-Path $CodexHome "codex-model-router\codex-router-cli.exe"
    if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) {
        $binary = Join-Path $CodexHome "codex-model-router\codex-router.exe"
    }
}
if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) {
    throw "Router binary missing. Extract the full package and install it first."
}

Write-Host "Enter the new DeepSeek API key. Input is hidden; an empty value cancels."
$secureKey = Read-Host -AsSecureString "New DeepSeek API Key"
if ($null -eq $secureKey -or $secureKey.Length -eq 0) {
    Write-Host "Cancelled. The existing key was kept."
    return
}
$pointer = [IntPtr]::Zero
$plainKey = $null
try {
    $pointer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secureKey)
    $plainKey = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($pointer)
    $plainKey | & $binary set-key --stdin --codex-home $CodexHome
    if ($LASTEXITCODE -ne 0) { throw "Key update failed with exit code $LASTEXITCODE" }
}
finally {
    $plainKey = $null
    if ($pointer -ne [IntPtr]::Zero) {
        [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($pointer)
    }
    $secureKey.Dispose()
}
