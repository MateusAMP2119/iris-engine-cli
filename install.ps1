# Iris installer for Windows: fetches the latest release binary for this platform.
# Windows PowerShell 5.1+ and PowerShell 7+. (UTF-8 with BOM: PS 5.1 needs the
# BOM to parse the banner's box-drawing glyphs.)
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
#
# Errors throw (never `exit`): under `irm | iex` an exit would close the
# caller's shell. Run as a file, an uncaught throw still yields exit code 1.

$ErrorActionPreference = 'Stop'

# Windows PowerShell 5.1 defaults to TLS 1.0; GitHub requires 1.2+.
if ($PSVersionTable.PSVersion.Major -lt 6) {
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
}
# Banner glyphs are multibyte; make sure they reach the console as UTF-8.
try { [Console]::OutputEncoding = [Text.Encoding]::UTF8 } catch {}

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

# Colors only on a real console, never under NO_COLOR. Legacy conhost (PS 5.1
# outside Windows Terminal) needs virtual-terminal processing switched on for
# the ANSI gradient; failure to enable it just means a plain banner.
$UseColor = (-not $env:NO_COLOR) -and (-not [Console]::IsOutputRedirected)
if ($UseColor -and $PSVersionTable.PSVersion.Major -lt 6 -and -not $env:WT_SESSION) {
    try {
        Add-Type -Namespace IrisInstall -Name Console -MemberDefinition @'
[DllImport("kernel32.dll", SetLastError = true)] public static extern IntPtr GetStdHandle(int nStdHandle);
[DllImport("kernel32.dll", SetLastError = true)] public static extern bool GetConsoleMode(IntPtr hConsoleHandle, out uint lpMode);
[DllImport("kernel32.dll", SetLastError = true)] public static extern bool SetConsoleMode(IntPtr hConsoleHandle, uint dwMode);
'@
        $handle = [IrisInstall.Console]::GetStdHandle(-11) # STD_OUTPUT_HANDLE
        $mode = 0
        if ([IrisInstall.Console]::GetConsoleMode($handle, [ref]$mode)) {
            # 0x4 = ENABLE_VIRTUAL_TERMINAL_PROCESSING
            $UseColor = [IrisInstall.Console]::SetConsoleMode($handle, $mode -bor 4)
        }
    } catch { $UseColor = $false }
}

$Esc = [char]27
# Ocean gradient rows, same RGB stops as install.sh (G1..G6).
$G = if ($UseColor) {
    @("$Esc[38;2;102;126;234m", "$Esc[38;2;105;115;219m", "$Esc[38;2;108;106;205m",
      "$Esc[38;2;112;95;191m", "$Esc[38;2;115;86;177m", "$Esc[38;2;118;75;162m")
} else { @('', '', '', '', '', '') }
$Rst = if ($UseColor) { "$Esc[0m" } else { '' }

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

# Banner: pre-rendered oh-my-logo art, 1:1 with install.sh — >=128 cols wide,
# >=92 stacked, else plain.
function Banner-Wide {
    Write-Host "$($G[0])  ██╗  ██████╗   ██╗  ███████╗       ██╗        █████╗   ██╗  ██╗  ███████╗  ██╗  ██╗   ██████╗   ██╗   ██╗  ███████╗  ███████╗$Rst"
    Write-Host "$($G[1])  ██║  ██╔══██╗  ██║  ██╔════╝       ██║       ██╔══██╗  ██║ ██╔╝  ██╔════╝  ██║  ██║  ██╔═══██╗  ██║   ██║  ██╔════╝  ██╔════╝$Rst"
    Write-Host "$($G[2])  ██║  ██████╔╝  ██║  ███████╗       ██║       ███████║  █████╔╝   █████╗    ███████║  ██║   ██║  ██║   ██║  ███████╗  █████╗$Rst"
    Write-Host "$($G[3])  ██║  ██╔══██╗  ██║  ╚════██║       ██║       ██╔══██║  ██╔═██╗   ██╔══╝    ██╔══██║  ██║   ██║  ██║   ██║  ╚════██║  ██╔══╝$Rst"
    Write-Host "$($G[4])  ██║  ██║  ██║  ██║  ███████║       ███████╗  ██║  ██║  ██║  ██╗  ███████╗  ██║  ██║  ╚██████╔╝  ╚██████╔╝  ███████║  ███████╗$Rst"
    Write-Host "$($G[5])  ╚═╝  ╚═╝  ╚═╝  ╚═╝  ╚══════╝       ╚══════╝  ╚═╝  ╚═╝  ╚═╝  ╚═╝  ╚══════╝  ╚═╝  ╚═╝   ╚═════╝    ╚═════╝   ╚══════╝  ╚══════╝$Rst"
}
function Banner-Stacked {
    Write-Host "$($G[0])  ██╗  ██████╗   ██╗  ███████╗$Rst"
    Write-Host "$($G[1])  ██║  ██╔══██╗  ██║  ██╔════╝$Rst"
    Write-Host "$($G[2])  ██║  ██████╔╝  ██║  ███████╗$Rst"
    Write-Host "$($G[3])  ██║  ██╔══██╗  ██║  ╚════██║$Rst"
    Write-Host "$($G[4])  ██║  ██║  ██║  ██║  ███████║$Rst"
    Write-Host "$($G[5])  ╚═╝  ╚═╝  ╚═╝  ╚═╝  ╚══════╝$Rst"
    Write-Host ''
    Write-Host "$($G[0])  ██╗        █████╗   ██╗  ██╗  ███████╗  ██╗  ██╗   ██████╗   ██╗   ██╗  ███████╗  ███████╗$Rst"
    Write-Host "$($G[1])  ██║       ██╔══██╗  ██║ ██╔╝  ██╔════╝  ██║  ██║  ██╔═══██╗  ██║   ██║  ██╔════╝  ██╔════╝$Rst"
    Write-Host "$($G[2])  ██║       ███████║  █████╔╝   █████╗    ███████║  ██║   ██║  ██║   ██║  ███████╗  █████╗$Rst"
    Write-Host "$($G[3])  ██║       ██╔══██║  ██╔═██╗   ██╔══╝    ██╔══██║  ██║   ██║  ██║   ██║  ╚════██║  ██╔══╝$Rst"
    Write-Host "$($G[4])  ███████╗  ██║  ██║  ██║  ██╗  ███████╗  ██║  ██║  ╚██████╔╝  ╚██████╔╝  ███████║  ███████╗$Rst"
    Write-Host "$($G[5])  ╚══════╝  ╚═╝  ╚═╝  ╚═╝  ╚═╝  ╚══════╝  ╚═╝  ╚═╝   ╚═════╝    ╚═════╝   ╚══════╝  ╚══════╝$Rst"
}

$Cols = 80
try { $Cols = $Host.UI.RawUI.WindowSize.Width } catch {}
if (-not $Cols -or $Cols -le 0) { $Cols = 80 }
Write-Host ''
if ($Cols -ge 128) { Banner-Wide }
elseif ($Cols -ge 92) { Banner-Stacked }
else { Write-Host "$($G[0])  IRIS LAKEHOUSE$Rst" }
Write-Host ''

$Arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { throw "iris: unsupported architecture: $($env:PROCESSOR_ARCHITECTURE)" }
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
    if (-not $WantLine) { throw "iris: checksums.txt has no entry for $Asset" }
    $Want = ($WantLine.Line -split '\s+')[0].ToLowerInvariant()
    $Got = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $Tmp $Asset)).Hash.ToLowerInvariant()
    if ($Want -ne $Got) { throw "iris: checksum mismatch for ${Asset}: want $Want, got $Got" }
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

    # Persistence: prepend to the user PATH in the registry when missing, then
    # broadcast WM_SETTINGCHANGE so terminals opened after this pick it up
    # without a relogin.
    $UserPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not $UserPath) { $UserPath = '' }
    $OnPath = ($UserPath -split ';' | Where-Object { $_ -eq $Dest }).Count -gt 0
    if (-not $OnPath -and -not $env:IRIS_DEST) {
        [Environment]::SetEnvironmentVariable('Path', "$Dest;$UserPath", 'User')
        try {
            Add-Type -Namespace IrisInstall -Name Env -MemberDefinition @'
[DllImport("user32.dll", SetLastError = true, CharSet = CharSet.Auto)]
public static extern IntPtr SendMessageTimeout(IntPtr hWnd, uint Msg, UIntPtr wParam, string lParam, uint fuFlags, uint uTimeout, out UIntPtr lpdwResult);
'@
            $result = [UIntPtr]::Zero
            # HWND_BROADCAST, WM_SETTINGCHANGE, SMTO_ABORTIFHUNG
            [IrisInstall.Env]::SendMessageTimeout([IntPtr]0xffff, 0x1A, [UIntPtr]::Zero, 'Environment', 2, 5000, [ref]$result) | Out-Null
        } catch {}
        Ok 'PATH configured'
    }
    # Same-shell availability: `irm | iex` and dot-/call-invoked runs share this
    # process, so `iris` works immediately in the invoking terminal too.
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
    if ($LASTEXITCODE -ne 0) { throw 'iris: engine setup failed' }

    Section '[4/4] Catalog'
    & $Bin setup --phase catalog
    if ($LASTEXITCODE -ne 0) { throw 'iris: catalog setup failed' }

    Write-Host ''
    Say 'Iris is ready! Try: iris --help'
    Write-Host ''
} finally {
    Remove-Item -Recurse -Force $Tmp -ErrorAction SilentlyContinue
}
