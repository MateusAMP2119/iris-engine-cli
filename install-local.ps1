# Build iris from the working tree, package it like a release asset, and run
# the repo's own install.ps1 against it (IRIS_BASE_URL=file://). 1:1 with
# `irm https://install.iris-lakehouse.bymarreco.com/snapshot.ps1 | iex`, local
# bits. Extra knobs pass through: IRIS_DEST, IRIS_ENGINE_SETUP,
# IRIS_SETUP_CATALOGS, NO_COLOR. Windows sibling of install-local.sh.
#
#   .\install-local.ps1; iris --version
#   .\install-local.ps1; iris uninstall --yes

$ErrorActionPreference = 'Stop'

$Root = (git rev-parse --show-toplevel).Trim()
$Dev = Join-Path $Root '.local'
New-Item -ItemType Directory -Path $Dev -Force | Out-Null

$Arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { Write-Error "install-local: unsupported architecture: $($env:PROCESSOR_ARCHITECTURE)"; exit 1 }
}

$Sha = (git -C $Root rev-parse --short=12 HEAD).Trim()
git -C $Root diff --quiet
$Dirty = if ($LASTEXITCODE -ne 0) { '-dirty' } else { '' }
$Version = "local.$(Get-Date -Format yyyyMMdd).$Sha$Dirty"
Write-Host "- Building $Version (windows/$Arch)"
$PriorCgo = $env:CGO_ENABLED
$env:CGO_ENABLED = '0'
go build -trimpath `
    -ldflags="-s -w -X github.com/MateusAMP2119/iris-lakehouse/internal/buildinfo.Version=$Version" `
    -o (Join-Path $Dev 'iris.exe') "$Root/cmd/iris"
if ($LASTEXITCODE -ne 0) { Write-Error 'install-local: go build failed'; exit 1 }

$Asset = "iris_windows_$Arch.zip"
Compress-Archive -Path (Join-Path $Dev 'iris.exe') -DestinationPath (Join-Path $Dev $Asset) -Force
$Hash = (Get-FileHash -Algorithm SHA256 (Join-Path $Dev $Asset)).Hash.ToLowerInvariant()
"$Hash  $Asset" | Out-File -Encoding ascii (Join-Path $Dev 'checksums.txt')

# file:// URL for Invoke-WebRequest: forward slashes, three-slash prefix.
$PriorBase = $env:IRIS_BASE_URL
$env:IRIS_BASE_URL = 'file:///' + ($Dev -replace '\\', '/')
# In-process (call operator, not a child powershell.exe): the installer's
# session-PATH update then lands in the invoking shell, so `iris` works
# immediately after this script returns. Restore the knobs this script set —
# same process means they would otherwise leak into the caller's shell.
try {
    & (Join-Path $Root 'install.ps1')
} finally {
    if ($null -eq $PriorBase) { Remove-Item Env:IRIS_BASE_URL -ErrorAction SilentlyContinue } else { $env:IRIS_BASE_URL = $PriorBase }
    if ($null -eq $PriorCgo) { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue } else { $env:CGO_ENABLED = $PriorCgo }
}
