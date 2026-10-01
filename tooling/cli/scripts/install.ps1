# Putnami CLI Install Script for Windows
# Usage, in a PowerShell window: irm https://putnami.dev/install.ps1 | iex
# Usage, from cmd.exe (Microsoft Defender blocks the line above passed to powershell -c):
#   curl.exe -fsSLo install.ps1 https://putnami.dev/install.ps1 && powershell -NoProfile -ExecutionPolicy Bypass -File install.ps1
# With flags, in PowerShell:
#   & ([scriptblock]::Create((irm https://putnami.dev/install.ps1))) --version canary
# To run one command, in a PowerShell window:
#   irm "https://putnami.dev/install.ps1?run=<command>" | iex
#
# This script installs the Putnami CLI on Windows into %USERPROFILE%\.putnami\bin.
# With a command to run (?run=, --run, or PUTNAMI_RUN), it then pins the extension
# that provides the command for the current user and runs `putnami <command>` in
# the caller's directory, which it never changes or writes to.
# On macOS and Linux, use https://putnami.dev/install.sh instead.
#
# Install trust model, the same as install.sh - see doc/22-installing-the-cli.md
# and ADR 0012:
#   - the download is compared against the SHA-256 the registry advertises for
#     it, and the word "verified" is printed only after that comparison passes;
#   - a download nobody vouched for is refused, not installed
#     (PUTNAMI_UNSAFE_INSTALL=1 overrides, loudly);
#   - the installed binary's own --version must match the version the registry
#     resolved, so a stale artifact cannot be installed under a fresh tag;
#   - the registry must be reachable over https, or loopback http, and
#     so must every redirect it answers with;
#   - the CLI is not downloaded before a writable install directory exists;
#   - the installer never escalates privileges. It writes only where the
#     invoking user can already write, and changes only the user's own PATH;
#   - a command to run is resolved from the command map before anything is
#     installed. The map is unsigned and served next to this script, over the
#     same https-or-loopback rule, so it is trusted exactly as far as this
#     script is. The extension it names is installed by the CLI, which verifies
#     that extension's SHA-256 like any other extension install.
#
# Prerequisites: Windows 10 version 1803 or later (for tar.exe), and Windows
# PowerShell 5.1 or PowerShell 7.
#
# Options: `irm | iex` passes the script no arguments, so set the environment
# variables that --help lists. The script block form above and a run as a file
# also take the same flags as install.sh.
#
# The file is ASCII only: Windows PowerShell 5.1 decodes a script without a byte
# order mark, and a download without a charset, in the ANSI code page, where a
# UTF-8 dash or quote becomes a different character.
#
# Everything runs inside one script block, so `irm | iex` leaves no function,
# variable or preference behind in the caller's session. A failure throws, which
# ends a `powershell -c` or `-File` run with exit code 1 and leaves an
# interactive session open. A command run with --run ends the script with the
# command's exit code: through exit when the script runs as a file, and in
# $LASTEXITCODE otherwise, since exit would close the window of a user who
# piped the script into iex.

& {
Set-StrictMode -Version 3.0
$ErrorActionPreference = 'Stop'

# Release settings
$CommandName = 'putnami'
$BinaryName = 'putnami.exe'
$DefaultRegistryUrl = 'https://put.putnami.dev'
$InstallScriptUrl = 'https://putnami.dev/install.ps1'
$UnixInstallScriptUrl = 'https://putnami.dev/install.sh'

# Supported host and binary targets. Anything else is refused, not warned about:
# a warning that is followed by an install produces a broken binary at the
# moment the user first runs it, with no evidence of why.
$SupportedMatrix = 'Windows 10 version 1803 or later and Windows 11, on amd64'

$UnsafeInstallEnv = 'PUTNAMI_UNSAFE_INSTALL'
$AllowInsecureRegistryEnv = 'PUTNAMI_ALLOW_INSECURE_REGISTRY'
$ExpectedSha256Env = 'PUTNAMI_EXPECTED_SHA256'
$NoAgentHostsEnv = 'PUTNAMI_NO_AGENT_HOSTS'

# The command to run after installing. The site sets the value of this one line
# for https://putnami.dev/install.ps1?run=<command> and changes nothing else in
# the script; --run and PUTNAMI_RUN take precedence over it.
$RunCommandDefault = ''

# The command map: which extension provides each command the installer can run.
# Its first line is $CommandMapHeader; every other line is blank, a # comment,
# or "<command> <@scope/name[@constraint]>". Anything else refuses the run.
$DefaultCommandMapUrl = 'https://putnami.dev/install-commands.txt'
$CommandMapUrlEnv = 'PUTNAMI_COMMAND_MAP_URL'
$CommandMapHeader = 'putnami.install-commands.v1'
$RunEnv = 'PUTNAMI_RUN'

# The patterns of install.sh, matched with -cmatch. A case-sensitive .NET range
# compares code points, so a-z is exactly the 26 lowercase ASCII letters, and
# \z anchors at the very end of the text, where $ also matches before a final
# newline.
# A command the installer can run: ^[a-z][a-z0-9-]{0,63}$. The site holds
# ?run= to the same rule before it sets $RunCommandDefault.
$RunCommandPattern = '^[a-z][a-z0-9-]{0,63}\z'
# An extension in the command map: @scope/name, optionally followed by
# @<constraint>, an optional ^ or ~ and then letters, digits, and . + _ -.
$ExtensionRefPattern = '^@[a-z0-9][a-z0-9._-]{0,127}/[a-z0-9][a-z0-9._-]{0,127}(@[~^]?[A-Za-z0-9][A-Za-z0-9.+_-]{0,63})?\z'
# One command map entry: two fields separated by blanks, nothing before or
# after. Blanks are spaces and tabs only: a carriage return is part of a field.
$CommandMapEntryPattern = '^([^ \t]+)[ \t]+([^ \t]+)\z'
# The largest command map the installer reads, in bytes.
$CommandMapMaxBytes = [long]1048576

# The same download cap `putnami upgrade` applies: a hostile or broken
# registry cannot fill the disk.
$MaxDownloadBytes = [long]500 * 1024 * 1024

# The binary switch contract `putnami upgrade` and `putnami version use` follow
# on Windows: an exclusive byte-range lock on this file serializes switches in a
# directory, and a replaced binary that may still run moves aside under a hidden
# name containing the marker, for a later switch to delete. The locked byte sits
# at 2^62, where the Putnami file locks place it.
$SwitchLockName = '.putnami-switch.lock'
$SwitchLockOffset = [long]4611686018427387904
$SwitchLockTimeoutSeconds = 120
$AsideMarker = '.old-'

# Whether this script block was read from a file (powershell -File install.ps1,
# or & .\install.ps1) rather than from text (irm | iex, or the script block
# form). Only a run from a file may end with exit: from text, exit ends the
# caller's session, which closes the window. The block's own file is asked, not
# $PSCommandPath, so text run by a caller's script never reads as a file.
$RunsFromFile = [bool]$MyInvocation.MyCommand.ScriptBlock.File

# Output. Diagnostics go to stderr, progress to the host. Color only when the
# stream is a console and NO_COLOR is unset: a redirected run is what CI and the
# tests read, and an escape sequence in the middle of a line breaks a grep.
#
# In run mode stdout belongs to the command, so progress goes to stderr too.
# The switch is a hashtable entry the entry point sets. A variable would need
# the $script: modifier, which under `irm | iex` names the caller's scope.
$Progress = @{ ToStderr = $false }

function Test-ColorEnabled([bool]$ErrorStream) {
    if ($env:NO_COLOR) { return $false }
    try {
        if ($ErrorStream) { return -not [Console]::IsErrorRedirected }
        return -not [Console]::IsOutputRedirected
    } catch {
        return $false
    }
}

function Write-OutLine([string]$Text, [string]$Color) {
    if ($Progress.ToStderr) {
        Write-ErrLine $Text $Color
        return
    }
    if ($Color -and (Test-ColorEnabled $false)) {
        Write-Host $Text -ForegroundColor $Color
    } else {
        Write-Host $Text
    }
}

function Write-ErrLine([string]$Text, [string]$Color) {
    if ($Color -and (Test-ColorEnabled $true)) {
        $previous = [Console]::ForegroundColor
        [Console]::ForegroundColor = $Color
        try { [Console]::Error.WriteLine($Text) } finally { [Console]::ForegroundColor = $previous }
    } else {
        [Console]::Error.WriteLine($Text)
    }
}

function Write-Step([string]$Text) { Write-OutLine "-> $Text" 'Cyan' }
function Write-Detail([string]$Text) { Write-OutLine "   $Text" '' }
function Write-Success([string]$Text) { Write-OutLine "[ok] $Text" 'Green' }
function Write-Info([string]$Text) { Write-OutLine $Text 'DarkGray' }
function Write-InstallError([string]$Text) { Write-ErrLine "[error] $Text" 'Red' }
function Write-Hint([string]$Text) { Write-ErrLine "   $Text" '' }
function Write-InstallWarning([string]$Text) { Write-ErrLine "[warn] $Text" 'Yellow' }

# Ends the installation after its cause has been printed.
function Stop-Install {
    throw 'Putnami CLI installation failed.'
}

# Runs a native command with its output captured as lines, and never throws on
# its exit code or its stderr: Windows PowerShell 5.1 turns a native stderr line
# into a terminating error under $ErrorActionPreference = 'Stop'. Output is
# decoded as UTF-8, which is what Go and Node write to a pipe, so a path with a
# non-ASCII user name reads back unchanged.
function Invoke-NativeCapture {
    param([string]$FilePath, [string[]]$ArgumentList, [switch]$MergeError)
    $ErrorActionPreference = 'Continue'
    $previousEncoding = $null
    try {
        $previousEncoding = [Console]::OutputEncoding
        [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
    } catch {
        $previousEncoding = $null
    }
    $lines = @()
    $code = -1
    try {
        if ($MergeError) {
            $lines = @(& $FilePath @ArgumentList 2>&1 | ForEach-Object { [string]$_ })
        } else {
            $lines = @(& $FilePath @ArgumentList 2>$null | ForEach-Object { [string]$_ })
        }
        $code = $LASTEXITCODE
    } catch {
        $lines = @([string]$_)
        $code = -1
    } finally {
        if ($null -ne $previousEncoding) {
            try { [Console]::OutputEncoding = $previousEncoding } catch { }
        }
    }
    return @{ ExitCode = $code; Lines = $lines }
}

# Platform seams. They are functions so the behavior around them can be
# exercised on any host; on Windows they read the real platform.
function Get-PutnamiPlatform {
    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
        return @{ OS = 'other'; Arch = ''; RawArch = '' }
    }
    # A 32-bit PowerShell on 64-bit Windows reports the machine in
    # PROCESSOR_ARCHITEW6432.
    $raw = [string]$env:PROCESSOR_ARCHITEW6432
    if (-not $raw) { $raw = [string]$env:PROCESSOR_ARCHITECTURE }
    $arch = 'unknown'
    if ($raw -eq 'AMD64') { $arch = 'amd64' }
    return @{ OS = 'windows'; Arch = $arch; RawArch = $raw }
}

# Whether this Windows enables TLS 1.3 for clients by default and this .NET can
# ask for it: Windows Server 2022 (build 20348), Windows 11 and later do.
function Test-Tls13ByDefault {
    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) { return $false }
    if ([Environment]::OSVersion.Version.Build -lt 20348) { return $false }
    return [Enum]::GetNames([Net.SecurityProtocolType]) -contains 'Tls13'
}

# The tar.exe Windows ships. Named by full path: a GNU tar from Git for Windows
# earlier on PATH reads C: in an archive path as a remote host.
function Get-SystemTarPath {
    $root = $env:SystemRoot
    if (-not $root) { $root = $env:windir }
    if (-not $root) { return '' }
    return [IO.Path]::Combine($root, 'System32', 'tar.exe')
}

function Get-UserProfileDirectory {
    return [string]$env:USERPROFILE
}

# Reads the user's persisted Path without expanding %VARIABLES%, so writing it
# back keeps them. Kind is the registry value kind, or '' when there is no value.
function Get-UserPathValue {
    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $false)
    if ($null -eq $key) { return @{ Value = $null; Kind = '' } }
    try {
        $value = $key.GetValue('Path', $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        if ($null -eq $value) { return @{ Value = $null; Kind = '' } }
        return @{ Value = [string]$value; Kind = [string]$key.GetValueKind('Path') }
    } finally {
        $key.Close()
    }
}

function Set-UserPathValue([string]$Value, [string]$Kind) {
    $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
    try {
        $key.SetValue('Path', $Value, [Microsoft.Win32.RegistryValueKind]$Kind)
    } finally {
        $key.Close()
    }
}

function Get-MachinePathValue {
    $key = [Microsoft.Win32.Registry]::LocalMachine.OpenSubKey('SYSTEM\CurrentControlSet\Control\Session Manager\Environment', $false)
    if ($null -eq $key) { return '' }
    try { return [string]$key.GetValue('Path', '') } finally { $key.Close() }
}

# Tells running programs, Explorer first, that the user environment changed, so
# a terminal opened from it afterwards sees the new Path. .NET broadcasts
# WM_SETTINGCHANGE after every user-scope write; removing a value that does not
# exist is that write without any other effect.
function Send-EnvironmentChange {
    [Environment]::SetEnvironmentVariable('PUTNAMI_INSTALLER_BROADCAST', $null, 'User')
}

# Takes the switch lock of a directory, waiting for another Putnami process that
# holds it. The lock is released when the returned stream closes.
function Enter-SwitchLock([string]$Directory) {
    $path = Join-Path $Directory $SwitchLockName
    $share = [IO.FileShare]::ReadWrite -bor [IO.FileShare]::Delete
    $stream = New-Object System.IO.FileStream($path, [IO.FileMode]::OpenOrCreate, [IO.FileAccess]::ReadWrite, $share)
    $deadline = [DateTime]::UtcNow.AddSeconds($SwitchLockTimeoutSeconds)
    $announced = $false
    while ($true) {
        try {
            $stream.Lock($SwitchLockOffset, 1)
            return $stream
        } catch {
            $cause = $_.Exception
            while ($null -ne $cause.InnerException -and -not ($cause -is [IO.IOException])) { $cause = $cause.InnerException }
            # ERROR_LOCK_VIOLATION: another process holds the byte.
            $busy = ($cause -is [IO.IOException]) -and (($cause.HResult -band 0xFFFF) -eq 33)
            if (-not $busy) {
                $stream.Dispose()
                throw
            }
            if ([DateTime]::UtcNow -ge $deadline) {
                $stream.Dispose()
                Write-InstallError "Another Putnami process still holds ${path} after $SwitchLockTimeoutSeconds seconds; nothing was installed."
                Write-Hint 'Let the running putnami upgrade or putnami version use finish, then re-run.'
                Stop-Install
            }
            if (-not $announced) {
                Write-Info "   Waiting for another Putnami process to finish switching binaries in ${Directory}..."
                $announced = $true
            }
            Start-Sleep -Milliseconds 200
        }
    }
}

function Exit-SwitchLock($Lock) {
    try { $Lock.Unlock($SwitchLockOffset, 1) } catch { } finally { $Lock.Dispose() }
}

function Get-TrimmedString([object]$Value) {
    if ($null -eq $Value) { return '' }
    return ([string]$Value).Trim()
}

# Replace any user:token@ in a URL with ***@ before it is printed.
# PUTNAMI_REGISTRY_URL is allowed to carry credentials, and CI logs keep what
# the installer prints.
function Get-RedactedUrl([string]$Url) {
    $scheme = ''
    $rest = $Url
    $separator = $Url.IndexOf('://')
    if ($separator -ge 0) {
        $scheme = $Url.Substring(0, $separator + 3)
        $rest = $Url.Substring($separator + 3)
    }
    $slash = $rest.IndexOf('/')
    $authority = $rest
    if ($slash -ge 0) { $authority = $rest.Substring(0, $slash) }
    $at = $authority.LastIndexOf('@')
    if ($at -ge 0) { $rest = '***@' + $rest.Substring($at + 1) }
    return $scheme + $rest
}

# Reject a URL that cannot carry an authenticated download: https is always
# fine, http only for loopback hosts or with an explicit opt-in, and no other
# scheme at all. The same rule as install.sh and `putnami upgrade`.
function Test-RegistryUrl([string]$Raw, [string]$Origin) {
    $separator = $Raw.IndexOf('://')
    if ($separator -lt 0) {
        Write-InstallError ("Invalid {0} ""{1}"": no scheme (expected https://...)" -f $Origin, (Get-RedactedUrl $Raw))
        return $false
    }
    $scheme = $Raw.Substring(0, $separator).ToLowerInvariant()
    $rest = $Raw.Substring($separator + 3)
    $slash = $rest.IndexOf('/')
    $authority = $rest
    if ($slash -ge 0) { $authority = $rest.Substring(0, $slash) }
    $at = $authority.LastIndexOf('@')
    if ($at -ge 0) { $authority = $authority.Substring($at + 1) }
    $hostName = $authority
    if ($authority.StartsWith('[') -and $authority.Contains(']')) {
        $hostName = $authority.Substring(0, $authority.IndexOf(']') + 1)
    } elseif ($authority.Contains(':')) {
        $hostName = $authority.Substring(0, $authority.IndexOf(':'))
    }
    $hostName = $hostName.ToLowerInvariant()

    if ($scheme -eq 'https') { return $true }
    if ($scheme -eq 'http') {
        if (@('localhost', '127.0.0.1', '::1', '[::1]') -contains $hostName) { return $true }
        if ([string][Environment]::GetEnvironmentVariable($AllowInsecureRegistryEnv) -eq '1') { return $true }
        Write-InstallError ("{0} ""{1}"" uses plaintext http://; refusing to fetch over an unauthenticated channel." -f $Origin, (Get-RedactedUrl $Raw))
        Write-Hint "Switch to https://, or set ${AllowInsecureRegistryEnv}=1 to override."
        return $false
    }
    Write-InstallError ("{0} ""{1}"" has unsupported scheme ""{2}"" (only https and http are accepted)." -f $Origin, (Get-RedactedUrl $Raw), $scheme)
    return $false
}

# Return the SHA-256 the registry advertised for the bytes it streamed, or ''
# when it advertised none: X-Integrity wins, otherwise the sha-256 member of an
# RFC 9530 Digest header. Only the response that carried the body counts.
function Get-AdvertisedIntegrity([hashtable]$Headers) {
    $value = Get-TrimmedString $Headers['x-integrity']
    if ($value) { return $value }
    $digest = [string]$Headers['digest']
    if (-not $digest) { return '' }
    foreach ($part in $digest.Split(',')) {
        $member = $part.Trim()
        if ($member.ToLowerInvariant().StartsWith('sha-256=')) {
            return $member.Substring($member.IndexOf('=') + 1).Trim()
        }
    }
    return ''
}

# Convert an advertised integrity string into a bare lowercase hex SHA-256, or
# $null after printing why it is not one. It accepts sha256:, sha-256:, sha256-
# and sha-256- prefixes and raw hex, and rejects everything else, so the
# comparison has one canonical path.
function ConvertTo-NormalizedIntegrity([string]$Raw, [string]$Origin) {
    $value = $Raw.Trim()
    $lowered = $value.ToLowerInvariant()
    foreach ($prefix in @('sha256:', 'sha-256:', 'sha256-', 'sha-256-')) {
        if ($lowered.StartsWith($prefix)) {
            $value = $value.Substring($prefix.Length)
            break
        }
    }
    $value = $value.Trim().ToLowerInvariant()
    if ($value.Length -ne 64) {
        Write-InstallError ("Invalid integrity from {0} (""{1}""): expected 64-character hex SHA-256, got {2} characters." -f $Origin, $Raw, $value.Length)
        return $null
    }
    if ($value -cnotmatch '^[0-9a-f]{64}$') {
        Write-InstallError ("Invalid integrity from {0} (""{1}""): not valid hex." -f $Origin, $Raw)
        return $null
    }
    return $value
}

# The lowercase hex SHA-256 of a file, or $null when this system cannot compute
# one.
function Get-FileSha256([string]$Path) {
    $algorithm = $null
    try {
        $algorithm = [Security.Cryptography.SHA256]::Create()
    } catch {
        try { $algorithm = New-Object System.Security.Cryptography.SHA256CryptoServiceProvider } catch { return $null }
    }
    $stream = [IO.File]::OpenRead($Path)
    try {
        $hash = $algorithm.ComputeHash($stream)
    } finally {
        $stream.Dispose()
        $algorithm.Dispose()
    }
    return ([BitConverter]::ToString($hash) -replace '-', '').ToLowerInvariant()
}

# Fail closed on a download whose authenticity nobody vouched for. The same rule
# as install.sh and the in-binary `putnami upgrade`.
function Confirm-DownloadIntegrity {
    param([string]$AssetFile, [string]$Advertised, [string]$ExpectedOverride, [string]$SourceLabel)

    $expected = ''
    if ($ExpectedOverride) {
        $expected = ConvertTo-NormalizedIntegrity $ExpectedOverride '--sha256'
        if ($null -eq $expected) { return $false }
    }

    $advertisedNormalized = ''
    if ($Advertised) {
        $advertisedNormalized = ConvertTo-NormalizedIntegrity $Advertised $SourceLabel
        if ($null -eq $advertisedNormalized) { return $false }
    }

    if ($expected -and $advertisedNormalized -and ($expected -cne $advertisedNormalized)) {
        Write-InstallError "The digest you supplied does not match the one $SourceLabel advertised; refusing to install."
        Write-Hint "--sha256:   $expected"
        Write-Hint "advertised: $advertisedNormalized"
        return $false
    }
    if (-not $expected) { $expected = $advertisedNormalized }

    $unsafe = [string][Environment]::GetEnvironmentVariable($UnsafeInstallEnv) -eq '1'
    if (-not $expected) {
        if (-not $unsafe) {
            Write-InstallError "$SourceLabel did not advertise an integrity hash; refusing to install an unverified binary."
            Write-Hint "Set ${UnsafeInstallEnv}=1 to override."
            Write-Hint "With --download-url, pass --sha256 <hex> or set ${ExpectedSha256Env}."
            return $false
        }
        Write-InstallWarning "Installing without integrity verification (${UnsafeInstallEnv}=1)."
        return $true
    }

    $actual = Get-FileSha256 $AssetFile
    if ($null -eq $actual) {
        if ($unsafe) {
            Write-InstallWarning "Installing without integrity verification (${UnsafeInstallEnv}=1): no SHA-256 implementation available."
            return $true
        }
        Write-InstallError 'Could not compute a SHA-256 for the download; refusing to install an unverified binary.'
        return $false
    }

    if ($actual -cne $expected) {
        Write-InstallError "Integrity check failed: expected $expected, got $actual"
        Write-Hint "The download does not match the digest $SourceLabel advertised. Nothing was installed."
        return $false
    }

    Write-Success "Integrity verified (sha256:$expected)"
    return $true
}

# The last field of the first line a binary prints for --version, or ''.
# NoRelaunch runs exactly this binary, never a workspace-pinned one it would
# otherwise hand over to.
function Get-BinaryVersionField {
    param([string]$BinaryPath, [switch]$NoRelaunch)
    $savedNoRelaunch = $env:PUTNAMI_NO_RELAUNCH
    $savedLaunched = $env:PUTNAMI_LAUNCHED
    try {
        if ($NoRelaunch) {
            $env:PUTNAMI_NO_RELAUNCH = '1'
            $env:PUTNAMI_LAUNCHED = $null
        }
        $result = Invoke-NativeCapture -FilePath $BinaryPath -ArgumentList @('--version')
    } finally {
        if ($NoRelaunch) {
            $env:PUTNAMI_NO_RELAUNCH = $savedNoRelaunch
            $env:PUTNAMI_LAUNCHED = $savedLaunched
        }
    }
    if ($result.Lines.Count -eq 0) { return '' }
    $line = Get-TrimmedString $result.Lines[0]
    if (-not $line) { return '' }
    $fields = @($line -split '\s+')
    return $fields[$fields.Count - 1]
}

# Refuse a binary whose embedded --version disagrees with the version the
# registry resolved. Best effort in one direction only: a binary
# that reports no version at all is not blocked.
function Confirm-BinaryStamp([string]$BinaryPath, [string]$ResolvedVersion) {
    if (-not $ResolvedVersion) { return $true }
    $reported = Get-BinaryVersionField -BinaryPath $BinaryPath -NoRelaunch
    if (-not $reported -or $reported -eq 'unknown') { return $true }
    if (($reported -creplace '^v', '') -ceq ($ResolvedVersion -creplace '^v', '')) {
        Write-Success "Version stamp verified ($reported)"
        return $true
    }
    Write-InstallError "The registry resolved $ResolvedVersion but the downloaded binary reports ""$reported""; refusing to install a stale build."
    Write-Hint "The release archives for $ResolvedVersion may not have been uploaded. Nothing was installed."
    return $false
}

# Normalize a version selector: semver inputs become v-prefixed (1.2.3 ->
# v1.2.3); channels and tags (canary, dev, latest) are kept as they are.
function ConvertTo-PutnamiTag([string]$Version) {
    if ($Version -ceq 'latest') { return 'latest' }
    if ($Version -cmatch '^v?[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9._-]+)?\z') {
        if ($Version.StartsWith('v')) { return $Version }
        return 'v' + $Version
    }
    return $Version
}

function Get-BasicAuthorization([Uri]$Uri) {
    if (-not $Uri.UserInfo) { return $null }
    $parts = $Uri.UserInfo.Split([char[]]@(':'), 2)
    $user = [Uri]::UnescapeDataString($parts[0])
    $password = ''
    if ($parts.Count -gt 1) { $password = [Uri]::UnescapeDataString($parts[1]) }
    return 'Basic ' + [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($user + ':' + $password))
}

function Test-SameOrigin([Uri]$Left, [Uri]$Right) {
    return ($Left.Scheme -eq $Right.Scheme) -and ($Left.Authority -eq $Right.Authority)
}

# The protocols a download may use, from the session's SecurityProtocol value:
# TLS 1.2 (3072) at least, and TLS 1.3 (12288) where it was allowed. A value
# that allows nothing older than TLS 1.2 is kept as it is. SystemDefault (0)
# lets Windows offer TLS 1.0 and 1.1 too, so it becomes TLS 1.2, plus TLS 1.3
# where Windows enables it by default.
function ConvertTo-TlsFloor([int]$Protocols, [bool]$Tls13ByDefault) {
    $tls12 = 3072
    $tls13 = 12288
    if ($Protocols -eq 0) {
        if ($Tls13ByDefault) { return $tls12 -bor $tls13 }
        return $tls12
    }
    if (($Protocols -band (-bnot ($tls12 -bor $tls13))) -eq 0) { return $Protocols }
    return $tls12 -bor ($Protocols -band $tls13)
}

# Download a URL to a file over TLS 1.2 or later, with the certificate checks
# of the system. Redirects are followed by hand, up to 10, and each target must
# pass the registry URL rule; credentials from the URL go only to its own
# origin. A body larger than MaxBytes is refused. Returns the lowercased
# headers of the response that carried the body, or an Error.
function Save-PutnamiDownload([string]$Url, [string]$OutFile, [long]$MaxBytes) {
    try {
        $current = New-Object System.Uri($Url)
    } catch {
        return @{ Error = 'not a valid URL'; Headers = @{} }
    }
    # Both settings are process-wide and a caller's session may have changed
    # them: a certificate callback can accept any certificate. The download
    # runs without a callback and with the TLS floor, and the caller's values
    # come back afterwards.
    $previousProtocols = [Net.ServicePointManager]::SecurityProtocol
    $previousCallback = [Net.ServicePointManager]::ServerCertificateValidationCallback
    try {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType](ConvertTo-TlsFloor ([int]$previousProtocols) (Test-Tls13ByDefault))
        [Net.ServicePointManager]::ServerCertificateValidationCallback = $null
        return Receive-PutnamiDownload $current $OutFile $MaxBytes
    } finally {
        [Net.ServicePointManager]::SecurityProtocol = $previousProtocols
        [Net.ServicePointManager]::ServerCertificateValidationCallback = $previousCallback
    }
}

function Receive-PutnamiDownload([Uri]$Url, [string]$OutFile, [long]$MaxBytes) {
    $current = $Url
    $origin = $current
    $authorization = Get-BasicAuthorization $current
    $redirects = 0
    while ($true) {
        $target = New-Object System.UriBuilder($current)
        $target.UserName = ''
        $target.Password = ''
        $request = [Net.HttpWebRequest][Net.WebRequest]::Create($target.Uri)
        $request.Method = 'GET'
        $request.AllowAutoRedirect = $false
        $request.UserAgent = 'putnami-install.ps1'
        $request.Timeout = 120000
        $request.ReadWriteTimeout = 120000
        if ($authorization -and (Test-SameOrigin $origin $current)) {
            $request.Headers['Authorization'] = $authorization
        }

        $response = $null
        try {
            $response = [Net.HttpWebResponse]$request.GetResponse()
        } catch {
            $cause = $_.Exception
            while ($null -ne $cause.InnerException -and -not ($cause -is [Net.WebException])) { $cause = $cause.InnerException }
            if (($cause -is [Net.WebException]) -and ($null -ne $cause.Response)) {
                $response = [Net.HttpWebResponse]$cause.Response
            } else {
                return @{ Error = $cause.Message; Headers = @{} }
            }
        }

        $status = [int]$response.StatusCode
        if (@(301, 302, 303, 307, 308) -contains $status) {
            $location = $response.Headers['Location']
            $response.Close()
            if (-not $location) { return @{ Error = "HTTP $status without a Location"; Headers = @{} } }
            if ($redirects -ge 10) { return @{ Error = 'stopped after 10 redirects'; Headers = @{} } }
            $redirects++
            $next = New-Object System.Uri($current, $location)
            if (-not (Test-RegistryUrl $next.AbsoluteUri 'redirect target')) {
                return @{ Error = 'refused redirect'; Headers = @{} }
            }
            $current = $next
            continue
        }
        if ($status -ne 200) {
            $response.Close()
            return @{ Error = "HTTP $status"; Headers = @{} }
        }

        $headers = @{}
        foreach ($name in $response.Headers.AllKeys) {
            $headers[$name.ToLowerInvariant()] = $response.Headers[$name]
        }
        $body = $response.GetResponseStream()
        $file = [IO.File]::Create($OutFile)
        try {
            $buffer = New-Object byte[] 81920
            $total = [long]0
            while (($read = $body.Read($buffer, 0, $buffer.Length)) -gt 0) {
                $total += $read
                if ($total -gt $MaxBytes) {
                    return @{ Error = ('download exceeds {0} MB limit' -f ($MaxBytes / 1MB)); Headers = @{} }
                }
                $file.Write($buffer, 0, $read)
            }
        } catch {
            return @{ Error = $_.Exception.Message; Headers = @{} }
        } finally {
            $file.Dispose()
            $body.Dispose()
            $response.Close()
        }
        return @{ Error = $null; Headers = $headers }
    }
}

# Extract the downloaded asset and return the path of the executable in it. The
# registry serves a .tar.gz with compiled\putnami.exe, or the raw executable;
# the format is sniffed from the first bytes, since the response carries no
# reliable file name.
function Expand-PutnamiAsset([string]$AssetFile, [string]$TempDir) {
    $magic = ''
    $stream = [IO.File]::OpenRead($AssetFile)
    try {
        $head = New-Object byte[] 2
        $count = $stream.Read($head, 0, 2)
        if ($count -eq 2) { $magic = '{0:x2}{1:x2}' -f $head[0], $head[1] }
    } finally {
        $stream.Dispose()
    }

    if ($magic -eq '1f8b') {
        $extractDir = Join-Path $TempDir 'extract'
        $null = [IO.Directory]::CreateDirectory($extractDir)
        $result = Invoke-NativeCapture -FilePath (Get-SystemTarPath) -ArgumentList @('-xf', $AssetFile, '-C', $extractDir) -MergeError
        if ($result.ExitCode -ne 0) {
            Write-InstallError 'Could not extract the downloaded archive.'
            foreach ($line in $result.Lines) { if ($line) { Write-Hint "tar: $line" } }
            Stop-Install
        }
        $found = Get-ChildItem -LiteralPath $extractDir -Recurse -File | Where-Object { $_.Name -eq $BinaryName } | Select-Object -First 1
        if ($null -ne $found) { return $found.FullName }
    } elseif ($magic -eq '4d5a') {
        $binary = Join-Path $TempDir $BinaryName
        [IO.File]::Copy($AssetFile, $binary, $true)
        return $binary
    }

    Write-InstallError "Downloaded asset did not contain a '$BinaryName' executable"
    Stop-Install
}

# Probe an actual write rather than trusting attributes: a read-only share, an
# ACL, or a full disk all look writable and then fail the install halfway.
function Test-DirectoryWritable([string]$Directory) {
    $probe = Join-Path $Directory ('.putnami-write-probe.' + $PID)
    try {
        [IO.File]::WriteAllBytes($probe, [byte[]]@())
        [IO.File]::Delete($probe)
        return $true
    } catch {
        return $false
    }
}

function Write-InstallDirRemedy {
    Write-Hint 'Pick a writable location with --install-dir <dir>, or set PUTNAMI_INSTALL_DIR=<dir>.'
    Write-Hint 'This installer never escalates privileges - run it as a user who can write to the target.'
}

# Resolve the install location before anything is downloaded, so a machine with
# no writable target fails in one second with a remedy.
function Assert-WritableInstallDir([string]$Directory) {
    if (Test-Path -LiteralPath $Directory -PathType Leaf) {
        Write-InstallError "Install directory $Directory exists and is not a directory."
        Write-InstallDirRemedy
        Stop-Install
    }
    if (-not (Test-Path -LiteralPath $Directory -PathType Container)) {
        try {
            $null = [IO.Directory]::CreateDirectory($Directory)
        } catch {
            Write-InstallError "Cannot create install directory $Directory (a parent is missing or not writable)."
            Write-InstallDirRemedy
            Stop-Install
        }
    }
    if (-not (Test-DirectoryWritable $Directory)) {
        Write-InstallError "Install directory $Directory is not writable by $([Environment]::UserName)."
        Write-InstallDirRemedy
        Stop-Install
    }
}

# Delete the binaries earlier switches moved aside in a directory. One that is
# still running refuses and stays for a later switch.
function Remove-AsideBinaries([string]$Directory) {
    foreach ($entry in [IO.Directory]::GetFiles($Directory)) {
        $name = [IO.Path]::GetFileName($entry)
        if ($name.StartsWith(".$CommandName") -and $name.Contains($AsideMarker)) {
            try { [IO.File]::Delete($entry) } catch { }
        }
    }
}

# Renames a file. Every move of the binary switch goes through here.
function Move-InstallFile([string]$From, [string]$To) {
    [IO.File]::Move($From, $To)
}

# Put a copy of Source at Destination, which may be a running executable.
# Windows refuses to delete or overwrite a running executable but lets it be
# renamed: the current file moves aside, the staged copy moves in, and the
# moved-aside file is deleted, or left for a later switch while it still runs.
# When the move in fails, the old file goes back. When that fails too, the old
# file stays aside and the error names it. Call under the switch lock.
function Install-ByRenameAside([string]$Source, [string]$Destination) {
    $directory = Split-Path -Parent $Destination
    $leaf = Split-Path -Leaf $Destination
    $staging = "$Destination.new.$PID"
    [IO.File]::Copy($Source, $staging, $true)
    try {
        $aside = $null
        if (Test-Path -LiteralPath $Destination) {
            $nanoseconds = ([DateTime]::UtcNow.Ticks - 621355968000000000) * 100
            $aside = Join-Path $directory ".$leaf$AsideMarker$PID-$nanoseconds"
            Move-InstallFile $Destination $aside
        }
        try {
            Move-InstallFile $staging $Destination
        } catch {
            $moveIn = $_
            if ($aside) {
                try {
                    Move-InstallFile $aside $Destination
                } catch {
                    Write-InstallError "Could not move the new $leaf into place ($($moveIn.Exception.Message)), nor the previous one back ($($_.Exception.Message))."
                    Write-Hint "The previous $leaf is kept at $aside."
                    Write-Hint "Move it back to $Destination before the next install or switch in $directory, which deletes it."
                    Stop-Install
                }
            }
            throw $moveIn
        }
        if ($aside) {
            try { [IO.File]::Delete($aside) } catch { }
        }
    } finally {
        if (Test-Path -LiteralPath $staging) { [IO.File]::Delete($staging) }
    }
}

# Install the binary under a versioned name and make putnami.exe a copy of it.
# This is the layout `putnami version use` and `putnami upgrade --global` manage
# on Windows, where a symbolic link would need Developer Mode.
function Install-VersionedBinary([string]$Source, [string]$BinDir, [string]$Variant, [string]$Tag) {
    $target = Join-Path $BinDir "putnami-$Variant-$Tag.exe"
    $lock = Enter-SwitchLock $BinDir
    try {
        Remove-AsideBinaries $BinDir
        Install-ByRenameAside $Source $target
        Install-ByRenameAside $target (Join-Path $BinDir $BinaryName)
    } finally {
        Exit-SwitchLock $lock
    }
    return $target
}

# Install the binary into an explicitly requested directory, under the plain
# name.
function Install-PlainBinary([string]$Source, [string]$Directory) {
    $target = Join-Path $Directory $BinaryName
    $lock = Enter-SwitchLock $Directory
    try {
        Remove-AsideBinaries $Directory
        Install-ByRenameAside $Source $target
    } finally {
        Exit-SwitchLock $lock
    }
    return $target
}

function ConvertTo-ComparablePath([string]$Entry) {
    $value = [Environment]::ExpandEnvironmentVariables($Entry.Trim().Trim('"'))
    if ($value.Length -gt 1) { $value = $value.TrimEnd('\', '/') }
    return $value
}

function Test-PathListContains([string]$PathList, [string]$Directory, [string]$Separator) {
    if (-not $PathList) { return $false }
    $wanted = ConvertTo-ComparablePath $Directory
    foreach ($entry in $PathList.Split($Separator)) {
        if (-not $entry.Trim()) { continue }
        if ((ConvertTo-ComparablePath $entry) -eq $wanted) { return $true }
    }
    return $false
}

# Put the directory first in the user's persisted Path. The value keeps its
# registry kind and its unexpanded %VARIABLES%. Returns whether it was written.
function Add-UserPathEntry([string]$Directory) {
    $current = Get-UserPathValue
    $kind = 'ExpandString'
    $rest = ''
    if ($null -ne $current.Value) {
        if (@('String', 'ExpandString') -notcontains $current.Kind) { return $false }
        $kind = $current.Kind
        $rest = $current.Value.Trim(';')
    }
    $updated = $Directory
    if ($rest) { $updated = "$Directory;$rest" }
    try {
        Set-UserPathValue $updated $kind
    } catch {
        return $false
    }
    try { Send-EnvironmentChange } catch { }
    return $true
}

# Make putnami reachable: in new terminals through the user's Path in
# HKCU\Environment (no administrator rights), and in this session through
# $env:PATH. Terminals that are already open keep their old Path, so the lines
# for them are printed.
function Add-PutnamiToPath([string]$Directory) {
    $user = Get-UserPathValue
    $persisted = (Test-PathListContains $user.Value $Directory ';') -or (Test-PathListContains (Get-MachinePathValue) $Directory ';')
    $inSession = Test-PathListContains $env:PATH $Directory ([string][IO.Path]::PathSeparator)

    if ($persisted -and $inSession) {
        Write-Success "$Directory is already on your PATH"
        return
    }
    if (-not $persisted) {
        if (Add-UserPathEntry $Directory) {
            Write-Success "Added $Directory to your user PATH (HKCU\Environment) for new terminals"
        } else {
            Write-InstallWarning "Could not update your user PATH; add $Directory to it by hand."
        }
    }
    if (-not $inSession) {
        $env:PATH = $Directory + [IO.Path]::PathSeparator + $env:PATH
    }
    Write-Info 'Terminals that were already open keep their old PATH. In one of them, run:'
    Write-OutLine "  PowerShell:  `$env:Path = ""$Directory;`$env:Path""" 'Cyan'
    Write-OutLine "  cmd.exe:     set ""PATH=$Directory;%PATH%""" 'Cyan'
}

# Say which binary `putnami` runs right now, and never claim more than that.
function Write-CommandResolution([string]$InstalledBinary) {
    $command = Get-Command -Name $CommandName -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -eq $command) { return }
    $resolved = [string]$command.Path
    if ([IO.Path]::GetFullPath($resolved) -eq [IO.Path]::GetFullPath($InstalledBinary)) {
        Write-Success "$CommandName now runs $resolved"
        return
    }
    Write-InstallWarning "$CommandName currently runs $resolved, not the build just installed ($InstalledBinary)."
    Write-Hint "Put $(Split-Path -Parent $InstalledBinary) earlier on your PATH, or remove the other copy."
}

# An agent host's own executable. Only applications count: a PowerShell shim
# script would run inside this session, with its preferences.
function Find-HostCommand([string]$Name) {
    $command = Get-Command -Name $Name -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -eq $command) { return $null }
    return [string]$command.Path
}

# Register Putnami's stable installed path with agent hosts through each host's
# supported CLI, as install.sh does. Host configuration is human-owned: an
# existing `putnami` definition is inspected and preserved, even when it points
# at a different command. Registration is best effort, because managed host
# policy may prohibit user configuration while still allowing the CLI install.
function Test-ClaudeDefinition([string[]]$Lines, [string]$InstalledBinary) {
    $environmentLines = @($Lines | Where-Object { $_ -cmatch '^    [^ ]' })
    return ($Lines -ccontains "  Command: $InstalledBinary") -and
        ($Lines -ccontains '  Args: mcp') -and
        ($Lines -ccontains '    PUTNAMI_AGENT_WORKSPACE=${CLAUDE_PROJECT_DIR:-.}') -and
        ($environmentLines.Count -eq 1)
}

function Write-ClaudeVerified([string[]]$Lines) {
    if (@($Lines | Where-Object { $_ -cmatch '^  Status: .*Connected$' }).Count -gt 0) {
        Write-Success 'Claude Code MCP configuration and connection verified for new sessions'
    } else {
        Write-Success 'Claude Code MCP configuration verified for new sessions'
        Write-Info 'MCP connection not yet verified; Claude Code will check it when a Putnami workspace opens.'
    }
}

function Register-ClaudeCode([string]$Claude, [string]$InstalledBinary) {
    $get = Invoke-NativeCapture -FilePath $Claude -ArgumentList @('mcp', 'get', 'putnami') -MergeError
    if (Test-ClaudeDefinition $get.Lines $InstalledBinary) {
        Write-ClaudeVerified $get.Lines
        return
    }
    if (($get.ExitCode -eq 0) -or ($get.Lines -ccontains 'putnami:')) {
        Write-InstallWarning 'Claude Code already defines a putnami MCP server; its human configuration was preserved.'
        Write-Hint 'Inspect the active definition with: claude mcp get putnami'
        return
    }

    $add = Invoke-NativeCapture -FilePath $Claude -ArgumentList @('mcp', 'add', '--scope', 'user', '--env', 'PUTNAMI_AGENT_WORKSPACE=${CLAUDE_PROJECT_DIR:-.}', '--transport', 'stdio', 'putnami', '--', $InstalledBinary, 'mcp') -MergeError
    if ($add.ExitCode -ne 0) {
        Write-InstallWarning 'Could not register Putnami with Claude Code; the CLI installation is still usable.'
        Write-Hint 'Claude Code may be managed by host policy. Run: claude mcp get putnami'
        return
    }

    $get = Invoke-NativeCapture -FilePath $Claude -ArgumentList @('mcp', 'get', 'putnami') -MergeError
    if (Test-ClaudeDefinition $get.Lines $InstalledBinary) {
        Write-ClaudeVerified $get.Lines
        return
    }
    Write-InstallWarning 'Claude Code accepted the MCP registration but did not report the expected Putnami launcher.'
    Write-Hint 'Inspect the active definition with: claude mcp get putnami'
}

# A field of Codex's JSON definition, at the top level or inside "transport",
# where current Codex releases keep the launcher.
function Get-CodexField([object]$Definition, [string]$Name) {
    $property = $Definition.PSObject.Properties[$Name]
    if ($null -ne $property) { return @{ Present = $true; Value = $property.Value } }
    $transport = $Definition.PSObject.Properties['transport']
    if (($null -ne $transport) -and ($transport.Value -is [System.Management.Automation.PSCustomObject])) {
        $property = $transport.Value.PSObject.Properties[$Name]
        if ($null -ne $property) { return @{ Present = $true; Value = $property.Value } }
    }
    return @{ Present = $false; Value = $null }
}

# Compare Codex's answer to the launcher contract by meaning, not by
# formatting: `codex mcp get --json` is a host-owned document that may gain
# fields, reorder keys or reindent. Only the fields that change behavior are
# asserted: a different command or argument is a different launcher, a stored
# cwd or env defeats per-session workspace resolution, and a disabled server or
# a tool filter is a deliberate human restriction.
function Test-CodexDefinition([string[]]$Lines, [string[]]$JsonLines, [string]$InstalledBinary) {
    if (-not (($Lines -ccontains "  command: $InstalledBinary") -and
            ($Lines -ccontains '  args: mcp') -and
            ($Lines -ccontains '  cwd: -') -and
            ($Lines -ccontains '  env: -'))) {
        return $false
    }
    try {
        $definition = ($JsonLines -join "`n") | ConvertFrom-Json
    } catch {
        return $false
    }
    if (-not ($definition -is [System.Management.Automation.PSCustomObject])) { return $false }

    $name = Get-CodexField $definition 'name'
    $command = Get-CodexField $definition 'command'
    $arguments = Get-CodexField $definition 'args'
    $enabled = Get-CodexField $definition 'enabled'
    $cwd = Get-CodexField $definition 'cwd'
    $environment = Get-CodexField $definition 'env'
    $environmentVars = Get-CodexField $definition 'env_vars'
    $enabledTools = Get-CodexField $definition 'enabled_tools'
    $disabledTools = Get-CodexField $definition 'disabled_tools'

    if (-not ($name.Present -and ($name.Value -is [string]) -and ($name.Value -ceq 'putnami'))) { return $false }
    if (-not ($command.Present -and ($command.Value -is [string]) -and ($command.Value -ceq $InstalledBinary))) { return $false }
    if (-not ($arguments.Present -and ($arguments.Value -is [array]) -and ($arguments.Value.Count -eq 1) -and ($arguments.Value[0] -ceq 'mcp'))) { return $false }
    if (-not ($enabled.Present -and ($enabled.Value -is [bool]) -and $enabled.Value)) { return $false }
    if (-not ($cwd.Present -and ($null -eq $cwd.Value))) { return $false }
    if ($environment.Present -and ($null -ne $environment.Value)) { return $false }
    if ($environmentVars.Present -and -not (($environmentVars.Value -is [array]) -and ($environmentVars.Value.Count -eq 0))) { return $false }
    if ($enabledTools.Present -and ($null -ne $enabledTools.Value)) { return $false }
    if ($disabledTools.Present -and ($null -ne $disabledTools.Value)) { return $false }
    return $true
}

function Test-CodexRegistration([string]$Codex, [string]$InstalledBinary) {
    $get = Invoke-NativeCapture -FilePath $Codex -ArgumentList @('mcp', 'get', 'putnami') -MergeError
    if ($get.ExitCode -ne 0) { return $false }
    $json = Invoke-NativeCapture -FilePath $Codex -ArgumentList @('mcp', 'get', 'putnami', '--json') -MergeError
    if ($json.ExitCode -ne 0) { return $false }
    return Test-CodexDefinition $get.Lines $json.Lines $InstalledBinary
}

function Register-Codex([string]$Codex, [string]$InstalledBinary) {
    $get = Invoke-NativeCapture -FilePath $Codex -ArgumentList @('mcp', 'get', 'putnami') -MergeError
    if ($get.ExitCode -eq 0) {
        if (Test-CodexRegistration $Codex $InstalledBinary) {
            Write-Success 'Codex MCP configuration verified for new sessions'
        } else {
            Write-InstallWarning 'Codex already defines a putnami MCP server; its human TOML and launcher were preserved.'
            Write-Hint 'Inspect the active definition with: codex mcp get putnami'
        }
        return
    }

    $add = Invoke-NativeCapture -FilePath $Codex -ArgumentList @('mcp', 'add', 'putnami', '--', $InstalledBinary, 'mcp') -MergeError
    if ($add.ExitCode -ne 0) {
        Write-InstallWarning 'Could not register Putnami with Codex; the CLI installation is still usable.'
        Write-Hint 'Codex may be managed by host policy. Run: codex mcp get putnami'
        return
    }

    if (Test-CodexRegistration $Codex $InstalledBinary) {
        Write-Success 'Codex MCP configuration verified for new sessions'
        return
    }
    Write-InstallWarning 'Codex accepted the MCP registration but did not report the expected Putnami launcher.'
    Write-Hint 'Inspect the active definition with: codex mcp get putnami'
}

function Register-AgentHosts([string]$InstalledBinary, [string]$NoAgentHosts) {
    if ($NoAgentHosts) {
        Write-Info "Skipping agent-host registration (${NoAgentHostsEnv}/--no-agent-hosts)."
        return
    }
    Write-Step 'Connecting supported agent hosts...'
    $claude = Find-HostCommand 'claude'
    if ($claude) {
        try {
            Register-ClaudeCode $claude $InstalledBinary
        } catch {
            Write-InstallWarning "Could not register Putnami with Claude Code; the CLI installation is still usable. ($($_.Exception.Message))"
        }
    }
    $codex = Find-HostCommand 'codex'
    if ($codex) {
        try {
            Register-Codex $codex $InstalledBinary
        } catch {
            Write-InstallWarning "Could not register Putnami with Codex; the CLI installation is still usable. ($($_.Exception.Message))"
        }
    }
}

# Report the installed CLI, then make it reachable and available to supported
# agent hosts. The CLI has no PowerShell completion, so none is installed.
function Complete-PutnamiInstall([string]$InstalledBinary, [string]$NoAgentHosts) {
    Write-Step 'Checking the installed CLI...'

    if (-not (Test-Path -LiteralPath $InstalledBinary -PathType Leaf)) {
        Write-InstallWarning "Binary installed but not executable: $InstalledBinary"
        return
    }

    $reported = Get-BinaryVersionField -BinaryPath $InstalledBinary
    if ($reported) {
        Write-Success "$CommandName $reported"
    } else {
        Write-Success "$CommandName installed"
    }

    Add-PutnamiToPath (Split-Path -Parent $InstalledBinary)
    Write-CommandResolution $InstalledBinary
    Register-AgentHosts $InstalledBinary $NoAgentHosts
}

# Return the extension that provides Command, as the command map text names it.
#
# The whole map is validated before any of it is used: a missing header, a
# malformed line anywhere, or a command listed twice refuses the run, so what a
# command resolves to never depends on where in the file a mistake sits. No
# value from the map is ever evaluated; it is only passed as one argument.
# Lines end at LF, as in install.sh: a carriage return stays in its line.
function Get-RunExtension([string]$MapText, [string]$Command, [string]$MapLabel) {
    if ($MapText.Length -eq 0) {
        Write-InstallError "$MapLabel is empty, not a command map. Refusing to run anything."
        Stop-Install
    }
    $lines = $MapText.Split([char]10)
    # A final LF ends the last line; it does not start another one.
    $lineCount = $lines.Count
    if ($MapText[$MapText.Length - 1] -eq [char]10) { $lineCount-- }
    # Compared ordinally, as install.sh compares bytes: -ceq compares by
    # culture, where a byte order mark before the header is invisible.
    if (-not [string]::Equals($lines[0], $CommandMapHeader, [StringComparison]::Ordinal)) {
        Write-InstallError "$MapLabel is not a command map: its first line is not $CommandMapHeader. Refusing to run anything."
        Stop-Install
    }

    $listed = @{}
    $resolved = ''
    for ($index = 1; $index -lt $lineCount; $index++) {
        $line = $lines[$index]
        $number = $index + 1
        # Blank lines and comments are ignored, indented or not. Blanks are
        # spaces and tabs only: a carriage return still makes a line malformed.
        $content = $line.TrimStart([char[]]@([char]32, [char]9))
        if (($content.Length -eq 0) -or $content.StartsWith('#', [StringComparison]::Ordinal)) { continue }
        $entry = [regex]::Match($line, $CommandMapEntryPattern)
        if (-not $entry.Success) {
            Write-InstallError "$MapLabel line $number is not ""<command> <@scope/name[@constraint]>"". Refusing to run anything."
            Stop-Install
        }
        $entryCommand = $entry.Groups[1].Value
        $entryRef = $entry.Groups[2].Value
        if ($entryCommand -cnotmatch $RunCommandPattern) {
            Write-InstallError ("$MapLabel line $number names a command that is not " + '^[a-z][a-z0-9-]{0,63}$. Refusing to run anything.')
            Stop-Install
        }
        if ($entryRef -cnotmatch $ExtensionRefPattern) {
            Write-InstallError "$MapLabel line $number names an extension that is not @scope/name[@constraint]. Refusing to run anything."
            Stop-Install
        }
        if ($listed.ContainsKey($entryCommand)) {
            Write-InstallError "$MapLabel lists $entryCommand twice (line $number). Refusing to guess which extension provides it."
            Stop-Install
        }
        $listed[$entryCommand] = $true
        if ([string]::Equals($entryCommand, $Command, [StringComparison]::Ordinal)) { $resolved = $entryRef }
    }

    if (-not $resolved) {
        Write-InstallError "putnami $Command is not a command the installer can run: $MapLabel does not list it."
        Write-Hint "Nothing was installed. To install the CLI alone: irm $InstallScriptUrl | iex"
        Stop-Install
    }
    return $resolved
}

# Download the command map and return the extension that provides Command. The
# map is held in a scratch file under %TEMP% only while it is read, and a map
# larger than $CommandMapMaxBytes is refused rather than read.
function Resolve-RunExtension([string]$MapUrl, [string]$Command) {
    $mapLabel = Get-RedactedUrl $MapUrl
    Write-OutLine '' ''
    Write-Step "Resolving putnami ${Command}..."
    Write-Detail "Command map: $mapLabel"
    $mapFile = Join-Path ([IO.Path]::GetTempPath()) ('putnami-install-' + [Guid]::NewGuid().ToString('N') + '.txt')
    try {
        $download = Save-PutnamiDownload $MapUrl $mapFile $CommandMapMaxBytes
        if ($download.Error) {
            Write-InstallError "Could not download the command map from $mapLabel"
            Write-Hint "download: $($download.Error)"
            Stop-Install
        }
        $mapText = [Text.Encoding]::UTF8.GetString([IO.File]::ReadAllBytes($mapFile))
    } finally {
        Remove-Item -LiteralPath $mapFile -Force -ErrorAction SilentlyContinue
    }
    $extension = Get-RunExtension $mapText $Command $mapLabel
    Write-Success "putnami $Command is provided by $extension"
    return $extension
}

# Ends run mode with Status: the command's, or the pin's when the pin failed.
# From a file the script exits with it, which is how cmd.exe and CI read a
# status. From text, exit would close the window of a user who piped the script
# into iex, so the status stays in $LASTEXITCODE, where PowerShell keeps the
# status of the last native command.
function Exit-RunMode([int]$Status) {
    $global:LASTEXITCODE = $Status
    if ($RunsFromFile) { exit $Status }
}

# Pin the extension that provides Command for the current user, then run
# `putnami <command>` in the caller's directory and end with its status.
#
# The pin passes --latest, so re-running the one line moves an existing pin to
# the newest release the map entry allows instead of keeping the first one. Its
# output joins the installer's messages on stderr.
#
# The command starts in the caller's current PowerShell location, as a command
# typed there does. Its output is not captured, so it reaches stdout as the
# command writes it, and it reads the console's stdin: in PowerShell the script
# never arrives on stdin, so the command cannot read it. Native stderr and a
# non-zero exit do not throw, whatever the caller's preferences: the status is
# the command's to report.
function Invoke-RequestedCommand([string]$Binary, [string]$Command, [string]$ExtensionRef) {
    $ErrorActionPreference = 'Continue'
    $PSNativeCommandUseErrorActionPreference = $false
    # Go writes UTF-8 to a pipe; PowerShell decodes what it captures with this.
    $previousEncoding = $null
    try {
        $previousEncoding = [Console]::OutputEncoding
        [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
    } catch {
        $previousEncoding = $null
    }
    try {
        Write-OutLine '' ''
        Write-Step "Pinning $ExtensionRef for your user..."
        $status = -1
        try {
            & $Binary extensions install --user --latest $ExtensionRef 2>&1 | ForEach-Object { [Console]::Error.WriteLine([string]$_) }
            $status = $LASTEXITCODE
        } catch {
            Write-InstallError "Could not run ${Binary}: $($_.Exception.Message)"
        }
        if ($status -ne 0) {
            Write-InstallError "putnami extensions install --user --latest $ExtensionRef failed (exit $status); putnami $Command was not run."
            Write-Hint "The CLI itself is installed: $Binary"
            Exit-RunMode $status
            return
        }
        Write-Success "Pinned $ExtensionRef"

        Write-OutLine '' ''
        Write-Step "Running putnami $Command in $((Get-Location -PSProvider FileSystem).ProviderPath)..."
        Write-OutLine '' ''
        $status = -1
        try {
            & $Binary $Command
            $status = $LASTEXITCODE
        } catch {
            Write-InstallError "Could not run ${Binary}: $($_.Exception.Message)"
        }
        Exit-RunMode $status
    } finally {
        if ($null -ne $previousEncoding) {
            try { [Console]::OutputEncoding = $previousEncoding } catch { }
        }
    }
}

function Write-Footer {
    Write-OutLine '' ''
    Write-OutLine 'Done' 'Green'
    Write-OutLine '' ''
    Write-OutLine 'Next:' ''
    Write-OutLine '  putnami --help' 'Cyan'
    Write-OutLine '  putnami init' 'Cyan'
    Write-OutLine '' ''
    Write-Info 'Docs:   putnami.dev'
    Write-Info 'GitHub: github.com/putnami/putnami'
    Write-OutLine '' ''
}

function Write-Usage {
    $row = '  {0,-33} {1}'
    Write-OutLine 'Putnami CLI Installer for Windows' ''
    Write-OutLine '' ''
    Write-OutLine "Usage: irm $InstallScriptUrl | iex" ''
    Write-OutLine "       irm ""${InstallScriptUrl}?run=<command>"" | iex" ''
    Write-OutLine "       & ([scriptblock]::Create((irm $InstallScriptUrl))) [options]" ''
    Write-OutLine '       powershell -ExecutionPolicy Bypass -File install.ps1 [options]' ''
    Write-OutLine '' ''
    Write-OutLine 'Options:' ''
    Write-OutLine '  --version <tag>       Install a specific version or channel (default: latest)' ''
    Write-OutLine '  --install-dir <dir>   Install directory (overrides the versioned layout)' ''
    Write-OutLine '  --variant <go|ts>     CLI variant to install (default: go)' ''
    Write-OutLine '  --download-url <url>  Direct URL to a binary asset (bypasses the registry)' ''
    Write-OutLine '  --sha256 <hex>        Expected SHA-256 of that asset (required with --download-url)' ''
    Write-OutLine '  --no-agent-hosts      Do not register Putnami with Claude Code or Codex' ''
    Write-OutLine '  --run <command>       Then pin the extension that provides <command> for your' ''
    Write-OutLine '                        user and run putnami <command> in the current directory' ''
    Write-OutLine '  --help, -h            Show this help message' ''
    Write-OutLine '' ''
    Write-OutLine 'irm | iex passes no options. Use the script block form above, or set the' ''
    Write-OutLine 'environment variables instead, for example:' ''
    Write-OutLine "  `$env:PUTNAMI_VERSION = 'canary'; irm $InstallScriptUrl | iex" ''
    Write-OutLine '' ''
    Write-OutLine 'Environment:' ''
    Write-OutLine ($row -f 'PUTNAMI_INSTALL_DIR', 'same as --install-dir') ''
    Write-OutLine ($row -f 'PUTNAMI_VERSION', 'same as --version') ''
    Write-OutLine ($row -f 'PUTNAMI_VARIANT', 'same as --variant') ''
    Write-OutLine ($row -f 'PUTNAMI_DOWNLOAD_URL', 'same as --download-url') ''
    Write-OutLine ($row -f $ExpectedSha256Env, 'same as --sha256') ''
    Write-OutLine ($row -f 'PUTNAMI_REGISTRY_URL', "registry base (default $DefaultRegistryUrl)") ''
    Write-OutLine ($row -f "${AllowInsecureRegistryEnv}=1", 'accept a plaintext http:// registry') ''
    Write-OutLine ($row -f "${UnsafeInstallEnv}=1", 'install without integrity verification') ''
    Write-OutLine ($row -f "${NoAgentHostsEnv}=1", 'same as --no-agent-hosts') ''
    Write-OutLine ($row -f $RunEnv, 'same as --run') ''
    Write-OutLine ($row -f $CommandMapUrlEnv, "command map for --run (default $DefaultCommandMapUrl)") ''
    Write-OutLine ($row -f 'NO_COLOR', 'disable colored output') ''
    Write-OutLine '' ''
    Write-OutLine 'The installer never escalates privileges: it writes only where you can' ''
    Write-OutLine 'already write, and refuses a download it cannot verify.' ''
    Write-OutLine '' ''
    Write-OutLine 'With --run, the command map names the extension that provides <command>; a' ''
    Write-OutLine 'command it does not list installs nothing. The installer''s messages go to' ''
    Write-OutLine 'stderr, the command''s output to stdout, and the exit code is the command''s:' ''
    Write-OutLine 'the exit code of a run as a file, and $LASTEXITCODE after irm | iex.' ''
    Write-OutLine 'Nothing is written to the current directory.' ''
    Write-OutLine '' ''
    Write-OutLine "Supported platforms: $SupportedMatrix." ''
    Write-OutLine "On macOS and Linux: curl -fsSL $UnixInstallScriptUrl | bash" ''
}

# Refuse any platform outside the supported matrix before anything is fetched.
function Assert-SupportedPlatform([hashtable]$Platform) {
    if ($Platform.OS -ne 'windows') {
        Write-InstallError 'install.ps1 installs the Putnami CLI on Windows only.'
        Write-Hint "On macOS and Linux, run: curl -fsSL $UnixInstallScriptUrl | bash"
        Stop-Install
    }
    if ($Platform.Arch -ne 'amd64') {
        Write-InstallError "Unsupported architecture: $($Platform.RawArch)"
        Write-Hint "Supported: $SupportedMatrix."
        Stop-Install
    }
}

# tar.exe ships with Windows 10 version 1803 and later; without it a release
# archive cannot be extracted.
function Assert-Prerequisites {
    $version = $PSVersionTable.PSVersion
    if (($version.Major -lt 5) -or (($version.Major -eq 5) -and ($version.Minor -lt 1))) {
        Write-InstallError "PowerShell 5.1 or later is required to install Putnami CLI (this is $version)."
        Stop-Install
    }
    $tar = Get-SystemTarPath
    if ((-not $tar) -or -not (Test-Path -LiteralPath $tar -PathType Leaf)) {
        Write-InstallError "tar.exe is required to extract Putnami CLI archives, and $tar does not exist."
        Write-Hint "Supported: $SupportedMatrix."
        Stop-Install
    }
}

function Invoke-PutnamiInstaller([string[]]$Arguments) {
    $version = [string]$env:PUTNAMI_VERSION
    if (-not $version) { $version = 'latest' }
    $installDir = [string]$env:PUTNAMI_INSTALL_DIR
    $downloadUrl = [string]$env:PUTNAMI_DOWNLOAD_URL
    $registryUrl = [string]$env:PUTNAMI_REGISTRY_URL
    if (-not $registryUrl) { $registryUrl = $DefaultRegistryUrl }
    $variant = [string]$env:PUTNAMI_VARIANT
    if (-not $variant) { $variant = 'go' }
    $expectedSha256 = [string][Environment]::GetEnvironmentVariable($ExpectedSha256Env)
    $noAgentHosts = [string][Environment]::GetEnvironmentVariable($NoAgentHostsEnv)
    $runCommand = [string][Environment]::GetEnvironmentVariable($RunEnv)
    if (-not $runCommand) { $runCommand = $RunCommandDefault }
    $commandMapUrl = [string][Environment]::GetEnvironmentVariable($CommandMapUrlEnv)
    if (-not $commandMapUrl) { $commandMapUrl = $DefaultCommandMapUrl }

    # Flags are spelled as in install.sh (--install-dir); the PowerShell
    # spellings (-InstallDir, -InstallDir:value) name the same options.
    $index = 0
    while ($index -lt $Arguments.Count) {
        $raw = [string]$Arguments[$index]
        $inline = $null
        $name = $raw
        if ($raw.StartsWith('-') -and $raw.Contains(':')) {
            $name = $raw.Substring(0, $raw.IndexOf(':'))
            $inline = $raw.Substring($raw.IndexOf(':') + 1)
        }
        $key = $name.TrimStart('-').Replace('-', '').ToLowerInvariant()
        if (-not $raw.StartsWith('-')) { $key = '' }

        if (@('help', 'h') -contains $key) {
            Write-Usage
            return
        }
        if ($key -eq 'noagenthosts') {
            $noAgentHosts = '1'
            $index++
            continue
        }
        $flags = @{ version = '--version'; installdir = '--install-dir'; downloadurl = '--download-url'; sha256 = '--sha256'; variant = '--variant'; run = '--run' }
        if (-not $flags.ContainsKey($key)) {
            Write-InstallError "Unknown option: $raw"
            Write-Hint 'Run with --help for the supported options.'
            Stop-Install
        }
        $value = $inline
        if (-not $value) {
            $value = ''
            if ($index + 1 -lt $Arguments.Count) { $value = [string]$Arguments[$index + 1] }
            $index += 2
        } else {
            $index++
        }
        if (-not $value) {
            if ($key -eq 'variant') {
                Write-InstallError '--variant requires a value (go or ts)'
            } elseif ($key -eq 'run') {
                Write-InstallError '--run requires a command'
            } else {
                Write-InstallError "$($flags[$key]) requires a value"
            }
            Stop-Install
        }
        switch ($key) {
            'version' { $version = $value }
            'installdir' { $installDir = $value }
            'downloadurl' { $downloadUrl = $value }
            'sha256' { $expectedSha256 = $value }
            'variant' { $variant = $value }
            'run' { $runCommand = $value }
        }
    }
    $tag = ConvertTo-PutnamiTag $version

    # The variant becomes part of the installed file name
    # (putnami-<variant>-<tag>.exe), so an unchecked typo installs a binary
    # under a name `putnami version use` never resolves.
    if (@('go', 'ts') -cnotcontains $variant) {
        Write-InstallError "Unknown variant ""$variant"" (expected go or ts)"
        Stop-Install
    }

    # Run mode: stdout belongs to the command, so the installer's own messages
    # go to stderr. The command is checked before anything else, including the
    # network.
    if ($runCommand) {
        $Progress.ToStderr = $true
        if ($runCommand -cnotmatch $RunCommandPattern) {
            Write-InstallError ("The command to run (--run, $RunEnv, or ?run=) must match " + '^[a-z][a-z0-9-]{0,63}$. Nothing was installed.')
            Stop-Install
        }
    }

    Assert-SupportedPlatform (Get-PutnamiPlatform)
    Assert-Prerequisites

    # An explicit --install-dir installs under the plain name, for callers that
    # manage the directory themselves. Otherwise the versioned
    # %USERPROFILE%\.putnami\bin layout is used, so `putnami version use` and
    # `upgrade` can switch between installs later.
    $useVersioned = $true
    if ($installDir) {
        $useVersioned = $false
        # Relative to the PowerShell location, which .NET does not follow.
        $installDir = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($installDir)
    } else {
        $userProfile = Get-UserProfileDirectory
        if (-not $userProfile) {
            Write-InstallError 'USERPROFILE is not set, so the default install directory is unknown.'
            Write-InstallDirRemedy
            Stop-Install
        }
        $installDir = Join-Path (Join-Path $userProfile '.putnami') 'bin'
    }

    # Validate the source before touching the filesystem: a bad URL is a typo to
    # fix, not a reason to create directories.
    $registryUrl = $registryUrl -replace '/$', ''
    if ($downloadUrl) {
        $valid = Test-RegistryUrl $downloadUrl 'download URL'
    } else {
        $valid = Test-RegistryUrl $registryUrl 'registry URL'
    }
    if ($valid -and $runCommand) {
        $valid = Test-RegistryUrl $commandMapUrl 'command map URL'
    }
    if (-not $valid) { Stop-Install }

    # A command the map does not list is a typo to fix, not a reason to create
    # directories or install a CLI: it is resolved before anything is written.
    $runExtension = ''
    if ($runCommand) {
        $runExtension = Resolve-RunExtension $commandMapUrl $runCommand
    }

    Assert-WritableInstallDir $installDir

    $tempDir = Join-Path ([IO.Path]::GetTempPath()) ('putnami-install-' + [Guid]::NewGuid().ToString('N'))
    $null = [IO.Directory]::CreateDirectory($tempDir)
    try {
        $assetFile = Join-Path $tempDir 'putnami.asset'

        Write-OutLine '' ''
        Write-Step 'Downloading Putnami CLI...'

        if ($downloadUrl) {
            $sourceLabel = 'the download URL'
            $downloadTarget = $downloadUrl
        } else {
            $sourceLabel = 'the registry'
            $downloadTarget = "$registryUrl/putnami/cli/download?channel=$([Uri]::EscapeDataString($tag))&os=windows&arch=amd64"
        }
        $displaySource = (Get-RedactedUrl $downloadTarget) -replace '^https?://', ''

        Write-Detail 'Platform: windows/amd64'
        Write-Detail "Channel:  $tag"
        Write-Detail "Source:   $displaySource"
        Write-Detail "Target:   $installDir"

        $download = Save-PutnamiDownload $downloadTarget $assetFile $MaxDownloadBytes
        if ($download.Error) {
            if ($downloadUrl) {
                Write-InstallError "Could not download $(Get-RedactedUrl $downloadUrl)"
            } else {
                Write-InstallError "No compatible release artifact found for windows/amd64 (version: $tag)"
                Write-Hint "Registry: $(Get-RedactedUrl $downloadTarget)"
                Write-Hint 'Set PUTNAMI_DOWNLOAD_URL or use --download-url to provide an explicit asset URL.'
            }
            Write-Hint "download: $($download.Error)"
            Stop-Install
        }

        Write-Success 'Downloaded'

        $advertisedIntegrity = Get-AdvertisedIntegrity $download.Headers
        $resolvedVersion = Get-TrimmedString $download.Headers['x-resolved-version']

        if (-not (Confirm-DownloadIntegrity $assetFile $advertisedIntegrity $expectedSha256 $sourceLabel)) {
            Stop-Install
        }

        $binarySource = Expand-PutnamiAsset $assetFile $tempDir

        # Bind the install to an exact version before anything lands in the
        # install directory, so a refusal leaves nothing behind.
        if (-not (Confirm-BinaryStamp $binarySource $resolvedVersion)) {
            Stop-Install
        }

        Write-OutLine '' ''
        Write-Step 'Installing Putnami CLI...'

        if ($useVersioned) {
            $installedBinary = Install-VersionedBinary $binarySource $installDir $variant $tag
            Write-Success "Installed to $installedBinary"
            Write-Success "Active: $BinaryName (a copy of $(Split-Path -Leaf $installedBinary))"
        } else {
            $installedBinary = Install-PlainBinary $binarySource $installDir
            Write-Success "Installed to $installedBinary"
        }

        Write-OutLine '' ''

        Complete-PutnamiInstall (Join-Path $installDir $BinaryName) $noAgentHosts

        if (-not $runCommand) { Write-Footer }
    } finally {
        Remove-Item -LiteralPath $tempDir -Recurse -Force -ErrorAction SilentlyContinue
    }

    if ($runCommand) {
        Invoke-RequestedCommand (Join-Path $installDir $BinaryName) $runCommand $runExtension
    }
}

Invoke-PutnamiInstaller ([string[]]$args)
} @args
