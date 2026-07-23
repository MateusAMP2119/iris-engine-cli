# Iris installer for Windows: fetches the latest release binary for this platform.
# Windows PowerShell 5.1+ and PowerShell 7+.
#
# Recommended:
#   irm https://install.iris-lakehouse.bymarreco.com/install.ps1 | iex
#   irm https://install.iris-lakehouse.bymarreco.com/snapshot.ps1 | iex
#
# Current (raw GitHub):
#   irm https://raw.githubusercontent.com/MateusAMP2119/iris-lakehouse/HEAD/install.ps1 | iex
#
# Knobs (environment variables; the pipe-to-iex form takes no parameters):
#   IRIS_VERSION=<tag>   release tag to install ("snapshot" -> rolling development build)
#   IRIS_BASE_URL=<url>  fetch the asset + checksums from here (local testing)
#   IRIS_DEST=<dir>      install into this directory (default ~\.iris\bin)
#   IRIS_ENGINE_SETUP=<local|remote|skip>          answer the engine-setup menu without a prompt
#   IRIS_SETUP_CATALOGS=<public|skip|url[,url...]>  answer the catalog menu without a prompt
#   NO_COLOR             plain output

$ErrorActionPreference = 'Stop'

# Windows PowerShell 5.1 defaults to TLS 1.0; GitHub requires 1.2+.
if ($PSVersionTable.PSVersion.Major -lt 6) {
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
}

$Repo = 'MateusAMP2119/iris-lakehouse'
$Requested = 'latest'
$Base = "https://github.com/$Repo/releases/latest/download"
if ($env:IRIS_VERSION) {
    $Requested = $env:IRIS_VERSION
    $Base = "https://github.com/$Repo/releases/download/$($env:IRIS_VERSION)"
}
if ($env:IRIS_BASE_URL) {
    $Base = $env:IRIS_BASE_URL
    $Requested = "$(if ($env:IRIS_VERSION) { $env:IRIS_VERSION } else { 'latest' }) (from $Base)"
}

$UseColor = (-not $env:NO_COLOR)
function Say([string]$Text) { Write-Host "  $Text" }
function Ok([string]$Text) {
    if ($UseColor) { Write-Host '  ' -NoNewline; Write-Host ([char]0x2713) -ForegroundColor Green -NoNewline; Write-Host " $Text" }
    else { Write-Host "  + $Text" }
}
function Warn([string]$Text) {
    if ($UseColor) { Write-Host '  ' -NoNewline; Write-Host '!' -ForegroundColor Yellow -NoNewline; Write-Host " $Text" }
    else { Write-Host "  ! $Text" }
}
function Section([string]$Text) {
    Write-Host ''
    if ($UseColor) { Write-Host "  $Text" -ForegroundColor Cyan } else { Write-Host "  $Text" }
}
function Kv([string]$Key, [string]$Value) { Write-Host ('  {0} {1,-15}: {2}' -f [char]0x2022, $Key, $Value) }

Write-Host ''
if ($UseColor) { Write-Host '  IRIS LAKEHOUSE' -ForegroundColor Blue } else { Write-Host '  IRIS LAKEHOUSE' }
Write-Host ''

$Arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { Write-Error "iris: unsupported architecture: $($env:PROCESSOR_ARCHITECTURE)"; exit 1 }
}
Say "Detected platform: windows/$Arch"

# Plan first, actions after; existing iris on PATH = announced upgrade.
$Installed = ''
$Existing = Get-Command iris -ErrorAction SilentlyContinue
if ($Existing) {
    try { $Installed = (& $Existing.Source --version) 2>$null } catch { $Installed = '' }
}
$Dest = if ($env:IRIS_DEST) { $env:IRIS_DEST } else { Join-Path $env:USERPROFILE '.iris\bin' }
Write-Host '  Install plan'
Kv 'OS/Arch' "windows / $Arch"
Kv 'Method' 'Prebuilt static binary'
Kv 'Version' $Requested
if ($Installed) { Kv 'Installed' "$Installed -> upgrading" }
Kv 'Destination' $(if ($env:IRIS_DEST) { $Dest } else { '~\.iris' })

$Asset = "iris_windows_$Arch.zip"
$Tmp = Join-Path ([IO.Path]::GetTempPath()) "iris-install-$([IO.Path]::GetRandomFileName())"
New-Item -ItemType Directory -Path $Tmp -Force | Out-Null
try {
    Section '[1/4] Downloading'
    Say "- Fetching $Asset"
    Invoke-WebRequest -UseBasicParsing -Uri "$Base/$Asset" -OutFile (Join-Path $Tmp $Asset)
    Invoke-WebRequest -UseBasicParsing -Uri "$Base/checksums.txt" -OutFile (Join-Path $Tmp 'checksums.txt')

    $WantLine = Select-String -Path (Join-Path $Tmp 'checksums.txt') -Pattern ([regex]::Escape($Asset) + '$') | Select-Object -First 1
    if (-not $WantLine) { Write-Error "iris: checksums.txt has no entry for $Asset"; exit 1 }
    $Want = ($WantLine.Line -split '\s+')[0].ToLowerInvariant()
    $Got = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $Tmp $Asset)).Hash.ToLowerInvariant()
    if ($Want -ne $Got) { Write-Error "iris: checksum mismatch for ${Asset}: want $Want, got $Got"; exit 1 }
    Ok 'Verifying checksum... Verified'

    Section '[2/4] Installing'
    Say '- Extracting binary...'
    Expand-Archive -Path (Join-Path $Tmp $Asset) -DestinationPath $Tmp -Force
    New-Item -ItemType Directory -Path $Dest -Force | Out-Null
    $Bin = Join-Path $Dest 'iris.exe'
    # A running iris.exe cannot be overwritten in place, but it can be renamed.
    if (Test-Path $Bin) {
        Remove-Item "$Bin.old" -ErrorAction SilentlyContinue
        try { Move-Item $Bin "$Bin.old" -Force } catch {}
    }
    Move-Item (Join-Path $Tmp 'iris.exe') $Bin -Force
    Remove-Item "$Bin.old" -ErrorAction SilentlyContinue
    Ok 'Binary installed'

    # Same-shell availability plus persistence: prepend to the user PATH in the
    # registry when missing, and always to this session's PATH.
    $UserPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not $UserPath) { $UserPath = '' }
    $OnPath = ($UserPath -split ';' | Where-Object { $_ -eq $Dest }).Count -gt 0
    if (-not $OnPath -and -not $env:IRIS_DEST) {
        [Environment]::SetEnvironmentVariable('Path', "$Dest;$UserPath", 'User')
        Ok 'PATH configured (new terminals pick it up automatically)'
    }
    if (($env:Path -split ';' | Where-Object { $_ -eq $Dest }).Count -eq 0) {
        $env:Path = "$Dest;$env:Path"
    }

    Section '[3/4] Engine Setup'
    if ($Requested -like 'snapshot*') {
        Warn 'Snapshot build - features may change; some are experimental.'
        Write-Host ''
    }

    # Setup phases live in the binary (huh + viper-backed config + BT bars).
    # IRIS_ENGINE_SETUP=local|remote|skip and IRIS_SETUP_CATALOGS=public|skip|url still work headless.
    & $Bin setup --phase engine
    if ($LASTEXITCODE -ne 0) { Write-Error 'iris: engine setup failed'; exit 1 }

    Section '[4/4] Catalog'
    & $Bin setup --phase catalog
    if ($LASTEXITCODE -ne 0) { Write-Error 'iris: catalog setup failed'; exit 1 }

    Write-Host ''
    Say 'Iris is ready! Try: iris --help'
    Write-Host ''
} finally {
    Remove-Item -Recurse -Force $Tmp -ErrorAction SilentlyContinue
}
