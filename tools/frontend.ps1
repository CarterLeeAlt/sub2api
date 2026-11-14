# Repository-only frontend entry. PowerShell 7 is the bootstrap prerequisite.
$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot
$manifest = Get-Content (Join-Path $root 'frontend/package.json') -Raw | ConvertFrom-Json
$version = $manifest.engines.node
$platform = if ($IsWindows) { 'win' } elseif ($IsMacOS) { 'darwin' } else { 'linux' }
$arch = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()
if ($arch -notin @('x64', 'arm64')) { throw "Unsupported architecture: $arch" }
$name = "node-v$version-$platform-$arch"
$node = Join-Path $root ".toolchains/$name/$(if ($IsWindows) { 'node.exe' } else { 'bin/node' })"
function Assert-ToolPath([string]$Target) {
    $current = [IO.Path]::GetFullPath($Target)
    while ($current -ne $root) {
        if (Test-Path -LiteralPath $current) {
            $item = Get-Item -LiteralPath $current -Force
            if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw "Toolchain links are not allowed: $current" }
        }
        $parent = Split-Path $current
        if (!$parent -or $parent -eq $current) { throw "Tool path outside repository: $Target" }
        $current = $parent
    }
}
Assert-ToolPath $node
if (!(Test-Path -LiteralPath $node)) {
    if ($args[0] -ne 'setup') { throw 'Local Node is missing. Run: pwsh -NoProfile -File tools/frontend.ps1 setup' }
    $downloads = Join-Path $root '.toolchains/downloads'
    Assert-ToolPath $downloads
    New-Item -ItemType Directory -Path $downloads -Force | Out-Null
    $extension = if ($IsWindows) { 'zip' } else { 'tar.gz' }
    $archive = Join-Path $downloads "$name.$extension"
    $sums = Join-Path $downloads "node-v$version-SHASUMS256.txt"
    $curl = if ($IsWindows) { 'curl.exe' } else { 'curl' }
    foreach ($item in @(@("https://nodejs.org/dist/v$version/$name.$extension", $archive), @("https://nodejs.org/dist/v$version/SHASUMS256.txt", $sums))) {
        & $curl --fail --show-error --location --output $item[1] $item[0]
        if ($LASTEXITCODE -ne 0) { throw 'Node download failed' }
    }
    $line = @(Get-Content $sums | Where-Object { $_.EndsWith("  $name.$extension") })
    if ($line.Count -ne 1 -or (Get-FileHash $archive -Algorithm SHA256).Hash.ToLowerInvariant() -ne ($line[0] -split ' ')[0]) { throw 'Node archive checksum mismatch' }
    & tar -xf $archive -C (Join-Path $root '.toolchains')
    if ($LASTEXITCODE -ne 0) { throw 'Node extraction failed' }
}
Assert-ToolPath $node
& $node (Join-Path $PSScriptRoot 'frontend.mjs') @args
exit $LASTEXITCODE
