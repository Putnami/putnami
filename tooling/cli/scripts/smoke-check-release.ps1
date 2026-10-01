# Post-release smoke check for the putnami CLI on Windows: the whole public
# golden path. It is the windows/amd64 port of smoke-check-release.sh, with the
# same legs, the same inputs and the same failure lines.
#
# It runs what a new Windows user runs, in this order, against the real download
# channel: install with the public one-liner (install.ps1), run as script text
# the way the install guide gives it, find the command the way the same session
# and a new terminal find it, initialize the public TypeScript web starter,
# serve it, answer one real HTTP request, and stop it the way Ctrl+C does. Any
# leg that fails, fails the release and prints the evidence for that leg.
#
# It guards the same regressions smoke-check-release.sh names, on Windows,
# plus the Windows contract of ADR 0052: the LF policy file
# init writes, and a stop request that ends the served starter.
#
# With SMOKE_RUN_COMMAND set, a last leg runs the run form of the installer,
# irm "<installer URL>?run=<command>" | iex, as script text in a fresh
# `git init` directory that is not a workspace. It requires exit status 0 and a
# directory git reports as untouched. The command must be listed in the command
# map published next to the installer and must not wait for input.
#
# Usage:   powershell -NoProfile -ExecutionPolicy Bypass -File smoke-check-release.ps1 [channel]
#          (default channel: latest)
# Env:
#   PUTNAMI_REGISTRY_URL  registry base   (default https://put.putnami.dev)
#   SMOKE_INSTALL_URL     installer URL (default: https://putnami.dev/install.ps1)
#   SMOKE_RETRIES         GET attempts before giving up (default 5)
#   SMOKE_STARTUP_TIMEOUT seconds to wait for starter readiness (default 180)
#   SMOKE_DIAGNOSTICS_DIR copy bounded failure evidence under this directory
#   SMOKE_RUN_COMMAND     also run this command through the installer's run form
#                         (default: unset, no run leg)
#
# Prerequisites, as documented for Windows consumers: Windows PowerShell 5.1 or
# PowerShell 7, Bun v1.4.0 or later, Git for Windows (git and sh), and
# LongPathsEnabled. The smoke names each one that is missing before it touches
# the release channel.
#
# The install leg writes the user's Path in HKCU\Environment, as install.ps1
# does for every user. The smoke puts the previous value back when it ends, and
# is meant for a host created for the run.
#
# The file is ASCII only: Windows PowerShell 5.1 reads a script without a byte
# order mark in the ANSI code page, where a UTF-8 dash or quote changes.

param([string]$Channel = 'latest')

Set-StrictMode -Version 3.0
$ErrorActionPreference = 'Stop'

# The PowerShell that runs this script, so every child runs the same edition.
function Get-SmokePowerShell {
    if ($PSVersionTable.PSEdition -eq 'Core') { return (Join-Path $PSHOME 'pwsh.exe') }
    return (Join-Path $PSHOME 'powershell.exe')
}

# The smoke rewrites this process's environment (USERPROFILE, PATH and more). A
# script started as .\smoke-check-release.ps1 runs inside the caller's session,
# so it runs itself again in a child PowerShell and leaves that session as it
# was.
if ($env:PUTNAMI_SMOKE_CHILD -ne '1') {
    $env:PUTNAMI_SMOKE_CHILD = '1'
    # A line the child writes to stderr must not stop this relay: the child's
    # exit status is the result.
    $ErrorActionPreference = 'Continue'
    $childStatus = 1
    try {
        & (Get-SmokePowerShell) -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $PSCommandPath $Channel
        $childStatus = $LASTEXITCODE
    } finally {
        Remove-Item -LiteralPath Env:PUTNAMI_SMOKE_CHILD -ErrorAction SilentlyContinue
    }
    exit $childStatus
}

if (-not $Channel) { $Channel = 'latest' }
$DiagnosticByteLimit = 262144
$SmokeFailed = 'putnami release smoke failed'
$PublicInstallerUrl = 'https://putnami.dev/install.ps1'
$LaunchDir = (Get-Location).ProviderPath

function Write-Smoke([string]$Text) {
    [Console]::Out.WriteLine($Text)
    [Console]::Out.Flush()
}

# The native helpers the smoke needs and PowerShell does not expose: a process
# started as the root of a new console process group with its output in a file,
# the CTRL_BREAK_EVENT that group receives when a user presses Ctrl+C, and the
# long form of a path. The code is C# 5, which Windows PowerShell 5.1 compiles.
$NativeSource = '
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
using System.Text;

namespace PutnamiSmoke
{
    public sealed class ChildProcess : IDisposable
    {
        [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
        private struct StartupInfo
        {
            public int cb;
            public string lpReserved;
            public string lpDesktop;
            public string lpTitle;
            public int dwX;
            public int dwY;
            public int dwXSize;
            public int dwYSize;
            public int dwXCountChars;
            public int dwYCountChars;
            public int dwFillAttribute;
            public int dwFlags;
            public short wShowWindow;
            public short cbReserved2;
            public IntPtr lpReserved2;
            public IntPtr hStdInput;
            public IntPtr hStdOutput;
            public IntPtr hStdError;
        }

        [StructLayout(LayoutKind.Sequential)]
        private struct StartupInfoEx
        {
            public StartupInfo StartupInfo;
            public IntPtr lpAttributeList;
        }

        [StructLayout(LayoutKind.Sequential)]
        private struct ProcessInformation
        {
            public IntPtr hProcess;
            public IntPtr hThread;
            public int dwProcessId;
            public int dwThreadId;
        }

        [StructLayout(LayoutKind.Sequential)]
        private struct SecurityAttributes
        {
            public int nLength;
            public IntPtr lpSecurityDescriptor;
            public int bInheritHandle;
        }

        [DllImport("kernel32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
        private static extern bool CreateProcess(string application, StringBuilder commandLine, IntPtr processAttributes, IntPtr threadAttributes, bool inheritHandles, uint flags, IntPtr environment, string directory, ref StartupInfoEx startupInfo, out ProcessInformation information);

        [DllImport("kernel32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
        private static extern IntPtr CreateFile(string name, uint access, uint share, ref SecurityAttributes attributes, uint disposition, uint flags, IntPtr template);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool InitializeProcThreadAttributeList(IntPtr list, int count, int flags, ref IntPtr size);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool UpdateProcThreadAttribute(IntPtr list, uint flags, IntPtr attribute, IntPtr value, IntPtr size, IntPtr previous, IntPtr returnSize);

        [DllImport("kernel32.dll")]
        private static extern void DeleteProcThreadAttributeList(IntPtr list);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool CloseHandle(IntPtr handle);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern uint WaitForSingleObject(IntPtr handle, uint milliseconds);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool GetExitCodeProcess(IntPtr handle, out uint code);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool TerminateProcess(IntPtr handle, uint code);

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool GenerateConsoleCtrlEvent(uint ctrlEvent, uint processGroupId);

        [DllImport("kernel32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
        private static extern uint GetLongPathName(string shortPath, StringBuilder longPath, uint size);

        private const uint GenericRead = 0x80000000;
        private const uint FileAppendData = 0x00000004;
        private const uint Synchronize = 0x00100000;
        private const uint ShareAll = 0x00000007;
        private const uint OpenExisting = 3;
        private const uint OpenAlways = 4;
        private const uint FileAttributeNormal = 0x80;
        private const int UseStdHandles = 0x00000100;
        private const uint CreateNewProcessGroup = 0x00000200;
        private const uint ExtendedStartupInfoPresent = 0x00080000;
        private const uint CtrlBreakEvent = 1;
        private static readonly IntPtr HandleListAttribute = new IntPtr(0x00020002);
        private static readonly IntPtr InvalidHandle = new IntPtr(-1);

        private IntPtr process;

        public int Id { get; private set; }

        private ChildProcess(IntPtr process, int id)
        {
            this.process = process;
            Id = id;
        }

        private static IntPtr OpenInheritable(string path, uint access, uint disposition)
        {
            SecurityAttributes attributes = new SecurityAttributes();
            attributes.nLength = Marshal.SizeOf(typeof(SecurityAttributes));
            attributes.bInheritHandle = 1;
            IntPtr handle = CreateFile(path, access, ShareAll, ref attributes, disposition, FileAttributeNormal, IntPtr.Zero);
            if (handle == InvalidHandle)
            {
                throw new Win32Exception(Marshal.GetLastWin32Error(), "open " + path);
            }
            return handle;
        }

        // Start runs commandLine through application, with the standard input
        // read from NUL and both output streams appended to logPath. The child
        // inherits exactly those two handles. In a new process group, the
        // child ignores Ctrl+C and receives the CTRL_BREAK_EVENT SendCtrlBreak
        // sends to its group.
        public static ChildProcess Start(string application, string commandLine, string directory, string logPath, bool newProcessGroup)
        {
            IntPtr input = OpenInheritable("NUL", GenericRead, OpenExisting);
            IntPtr output = IntPtr.Zero;
            IntPtr list = IntPtr.Zero;
            IntPtr handles = IntPtr.Zero;
            bool listReady = false;
            try
            {
                output = OpenInheritable(logPath, FileAppendData | Synchronize, OpenAlways);
                IntPtr size = IntPtr.Zero;
                InitializeProcThreadAttributeList(IntPtr.Zero, 1, 0, ref size);
                list = Marshal.AllocHGlobal(size);
                if (!InitializeProcThreadAttributeList(list, 1, 0, ref size))
                {
                    throw new Win32Exception(Marshal.GetLastWin32Error(), "initialize the attribute list");
                }
                listReady = true;
                handles = Marshal.AllocHGlobal(IntPtr.Size * 2);
                Marshal.WriteIntPtr(handles, 0, input);
                Marshal.WriteIntPtr(handles, IntPtr.Size, output);
                if (!UpdateProcThreadAttribute(list, 0, HandleListAttribute, handles, new IntPtr(IntPtr.Size * 2), IntPtr.Zero, IntPtr.Zero))
                {
                    throw new Win32Exception(Marshal.GetLastWin32Error(), "set the inherited handles");
                }

                StartupInfoEx startup = new StartupInfoEx();
                startup.StartupInfo.cb = Marshal.SizeOf(typeof(StartupInfoEx));
                startup.StartupInfo.dwFlags = UseStdHandles;
                startup.StartupInfo.hStdInput = input;
                startup.StartupInfo.hStdOutput = output;
                startup.StartupInfo.hStdError = output;
                startup.lpAttributeList = list;
                uint flags = ExtendedStartupInfoPresent;
                if (newProcessGroup)
                {
                    flags |= CreateNewProcessGroup;
                }
                ProcessInformation information;
                if (!CreateProcess(application, new StringBuilder(commandLine), IntPtr.Zero, IntPtr.Zero, true, flags, IntPtr.Zero, directory, ref startup, out information))
                {
                    throw new Win32Exception(Marshal.GetLastWin32Error(), "start " + application);
                }
                CloseHandle(information.hThread);
                return new ChildProcess(information.hProcess, information.dwProcessId);
            }
            finally
            {
                if (listReady)
                {
                    DeleteProcThreadAttributeList(list);
                }
                if (list != IntPtr.Zero)
                {
                    Marshal.FreeHGlobal(list);
                }
                if (handles != IntPtr.Zero)
                {
                    Marshal.FreeHGlobal(handles);
                }
                if (output != IntPtr.Zero)
                {
                    CloseHandle(output);
                }
                CloseHandle(input);
            }
        }

        public bool WaitForExit(int milliseconds)
        {
            return WaitForSingleObject(process, (uint)milliseconds) == 0;
        }

        public bool HasExited
        {
            get { return WaitForExit(0); }
        }

        public int ExitCode
        {
            get
            {
                uint code;
                if (!GetExitCodeProcess(process, out code))
                {
                    throw new Win32Exception(Marshal.GetLastWin32Error(), "read the exit code");
                }
                return unchecked((int)code);
            }
        }

        // SendCtrlBreak returns 0, or the Win32 error that kept the event from
        // being sent: the child must share this console.
        public int SendCtrlBreak()
        {
            if (GenerateConsoleCtrlEvent(CtrlBreakEvent, (uint)Id))
            {
                return 0;
            }
            return Marshal.GetLastWin32Error();
        }

        public void Kill()
        {
            TerminateProcess(process, 1);
        }

        public void Dispose()
        {
            if (process != IntPtr.Zero)
            {
                CloseHandle(process);
                process = IntPtr.Zero;
            }
        }

        public static string LongPath(string path)
        {
            StringBuilder buffer = new StringBuilder(32768);
            uint length = GetLongPathName(path, buffer, (uint)buffer.Capacity);
            if (length == 0 || length >= buffer.Capacity)
            {
                return path;
            }
            return buffer.ToString();
        }
    }
}
'

# A leg failure: the ::error:: line, the evidence for it, then the stop. The
# top level catches the stop, cleans up, and exits 1.
function Stop-Smoke([string]$Message, [string]$EvidencePath) {
    Write-Smoke "::error::$Message"
    if ($EvidencePath) { Write-LogTail $EvidencePath 200 }
    throw $SmokeFailed
}

# Reads a file another process may still be writing.
function Read-SharedText([string]$Path) {
    $share = [IO.FileShare]::ReadWrite -bor [IO.FileShare]::Delete
    $stream = [IO.File]::Open($Path, [IO.FileMode]::Open, [IO.FileAccess]::Read, $share)
    try {
        $reader = New-Object IO.StreamReader($stream, (New-Object Text.UTF8Encoding($false)))
        return $reader.ReadToEnd()
    } finally {
        $stream.Dispose()
    }
}

function Get-TextLines([string]$Text) {
    $lines = @($Text -split "`r?`n")
    if ($lines.Count -gt 0 -and $lines[$lines.Count - 1] -eq '') {
        if ($lines.Count -eq 1) { return @() }
        $lines = $lines[0..($lines.Count - 2)]
    }
    return $lines
}

function Get-LogTail([string]$Path, [int]$Count) {
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return @() }
    $lines = @(Get-TextLines (Read-SharedText $Path))
    if ($lines.Count -le $Count) { return $lines }
    return $lines[($lines.Count - $Count)..($lines.Count - 1)]
}

function Write-LogTail([string]$Path, [int]$Count) {
    foreach ($line in (Get-LogTail $Path $Count)) { Write-Smoke $line }
}

function Get-Sha256([string]$Path) {
    return (Get-FileHash -Algorithm SHA256 -LiteralPath $Path).Hash.ToLowerInvariant()
}

function Get-FullPath([string]$Path) {
    return [PutnamiSmoke.ChildProcess]::LongPath([IO.Path]::GetFullPath($Path))
}

function Test-PathInside([string]$Path, [string]$Directory) {
    $prefix = (Get-FullPath $Directory).TrimEnd('\') + '\'
    return (Get-FullPath $Path).StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)
}

function Set-SmokeEnv([string]$Name, [string]$Value) {
    [Environment]::SetEnvironmentVariable($Name, $Value, 'Process')
}

# The command line a shell passes for CommandLine, whose first word names the
# program as a user types it: PowerShell replaces that word with the quoted
# path of the application it resolved.
function Get-CommandLine([string]$Application, [string]$CommandLine) {
    $space = $CommandLine.IndexOf(' ')
    if ($space -lt 0) { return '"' + $Application + '"' }
    return '"' + $Application + '"' + $CommandLine.Substring($space)
}

# Runs a command line to completion, with its output appended to LogPath, and
# returns its exit code.
function Invoke-SmokeCommand([string]$Application, [string]$CommandLine, [string]$Directory, [string]$LogPath) {
    $child = [PutnamiSmoke.ChildProcess]::Start($Application, (Get-CommandLine $Application $CommandLine), $Directory, $LogPath, $false)
    try {
        [void]$child.WaitForExit(-1)
        return $child.ExitCode
    } finally {
        $child.Dispose()
    }
}

# GET a URL without following redirects and without a proxy, like curl without
# -L. Returns the status (0 when nothing answered), the lowercased headers, and
# the error text.
function Invoke-SmokeGet([string]$Url, [string]$OutFile, [int]$TimeoutMilliseconds) {
    $request = [Net.HttpWebRequest][Net.WebRequest]::Create($Url)
    $request.Method = 'GET'
    $request.AllowAutoRedirect = $false
    $request.Proxy = $null
    $request.KeepAlive = $false
    $request.Timeout = $TimeoutMilliseconds
    $request.ReadWriteTimeout = $TimeoutMilliseconds
    $request.UserAgent = 'putnami-smoke-check-release.ps1'
    $response = $null
    try {
        $response = [Net.HttpWebResponse]$request.GetResponse()
    } catch {
        $cause = $_.Exception
        while ($null -ne $cause.InnerException -and -not ($cause -is [Net.WebException])) { $cause = $cause.InnerException }
        if (($cause -is [Net.WebException]) -and ($null -ne $cause.Response)) {
            $response = [Net.HttpWebResponse]$cause.Response
        } else {
            return @{ Status = 0; Headers = @{}; Error = $cause.Message }
        }
    }
    try {
        $headers = @{}
        foreach ($name in $response.Headers.AllKeys) { $headers[$name.ToLowerInvariant()] = [string]$response.Headers[$name] }
        $body = $response.GetResponseStream()
        $file = [IO.File]::Create($OutFile)
        try {
            $body.CopyTo($file)
        } finally {
            $file.Dispose()
            $body.Dispose()
        }
        return @{ Status = [int]$response.StatusCode; Headers = $headers; Error = '' }
    } catch {
        return @{ Status = 0; Headers = @{}; Error = $_.Exception.Message }
    } finally {
        $response.Close()
    }
}

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

function Get-MachinePathValue {
    $key = [Microsoft.Win32.Registry]::LocalMachine.OpenSubKey('SYSTEM\CurrentControlSet\Control\Session Manager\Environment', $false)
    if ($null -eq $key) { return '' }
    try { return [string]$key.GetValue('Path', '') } finally { $key.Close() }
}

# Puts the user's Path back as it was before the install leg, or removes it when
# there was none, then tells running programs the environment changed.
function Restore-UserPath {
    if ($null -eq $script:SavedUserPath) { return }
    $saved = $script:SavedUserPath
    $script:SavedUserPath = $null
    $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
    try {
        if ($null -eq $saved.Value) {
            $key.DeleteValue('Path', $false)
        } else {
            $key.SetValue('Path', $saved.Value, [Microsoft.Win32.RegistryValueKind]$saved.Kind)
        }
    } finally {
        $key.Close()
    }
    try { [Environment]::SetEnvironmentVariable('PUTNAMI_SMOKE_BROADCAST', $null, 'User') } catch { }
}

# The first file a Path list resolves Name to, trying every PATHEXT extension in
# each directory, as cmd.exe and a new terminal do. '' when nothing matches.
function Find-CommandInPathList([string]$PathList, [string]$Name) {
    $extensions = @('.COM', '.EXE', '.BAT', '.CMD')
    if ($env:PATHEXT) { $extensions = @($env:PATHEXT.Split(';') | Where-Object { $_ }) }
    foreach ($entry in $PathList.Split(';')) {
        $directory = [Environment]::ExpandEnvironmentVariables($entry.Trim().Trim('"'))
        if (-not $directory) { continue }
        foreach ($extension in $extensions) {
            $candidate = Join-Path $directory ($Name + $extension)
            if (Test-Path -LiteralPath $candidate -PathType Leaf) { return $candidate }
        }
    }
    return ''
}

# The executable the channel serves: the raw executable, or compiled\putnami.exe
# inside the .tar.gz archive, extracted with the tar.exe Windows ships.
function Get-ChannelExecutable([string]$AssetPath) {
    $stream = [IO.File]::OpenRead($AssetPath)
    $magic = ''
    try {
        $head = New-Object byte[] 2
        if ($stream.Read($head, 0, 2) -eq 2) { $magic = '{0:x2}{1:x2}' -f $head[0], $head[1] }
    } finally {
        $stream.Dispose()
    }
    if ($magic -eq '4d5a') { return $AssetPath }
    if ($magic -ne '1f8b') { return '' }
    $extractDir = Join-Path $Workdir 'asset-extract'
    $null = [IO.Directory]::CreateDirectory($extractDir)
    $tar = Join-Path $env:SystemRoot 'System32\tar.exe'
    $code = Invoke-SmokeCommand $tar ('tar -xf "{0}" -C "{1}"' -f $AssetPath, $extractDir) $Workdir (Join-Path $Workdir 'asset-extract.log')
    if ($code -ne 0) { return '' }
    $found = Get-ChildItem -LiteralPath $extractDir -Recurse -File | Where-Object { $_.Name -eq 'putnami.exe' } | Select-Object -First 1
    if ($null -eq $found) { return '' }
    return $found.FullName
}

# Writes the first MaxLines lines of Source, and at most the diagnostic byte
# limit, to Destination.
function Write-BoundedHead([string]$Source, [string]$Destination, [int]$MaxLines) {
    $lines = @(Get-TextLines (Read-SharedText $Source))
    if ($lines.Count -gt $MaxLines) { $lines = $lines[0..($MaxLines - 1)] }
    $text = ''
    if ($lines.Count -gt 0) { $text = ($lines -join "`n") + "`n" }
    $bytes = (New-Object Text.UTF8Encoding($false)).GetBytes($text)
    if ($bytes.Length -gt $DiagnosticByteLimit) { $bytes = $bytes[0..($DiagnosticByteLimit - 1)] }
    [IO.File]::WriteAllBytes($Destination, [byte[]]$bytes)
}

# Writes the last 200 lines of Source, and at most the diagnostic byte limit of
# them, to Destination.
function Write-BoundedTail([string]$Source, [string]$Destination) {
    $lines = @(Get-LogTail $Source 200)
    $text = ''
    if ($lines.Count -gt 0) { $text = ($lines -join "`n") + "`n" }
    $bytes = (New-Object Text.UTF8Encoding($false)).GetBytes($text)
    if ($bytes.Length -gt $DiagnosticByteLimit) { $bytes = $bytes[($bytes.Length - $DiagnosticByteLimit)..($bytes.Length - 1)] }
    [IO.File]::WriteAllBytes($Destination, [byte[]]$bytes)
}

function Save-FailureArtifacts {
    if (-not $DiagnosticsDir) { return }
    $safeChannel = $Channel -replace '[^A-Za-z0-9._-]', '_'
    if (-not $safeChannel) { $safeChannel = 'channel' }
    $target = Join-Path $DiagnosticsDir "$safeChannel-windows-amd64"
    $null = [IO.Directory]::CreateDirectory($target)
    foreach ($log in @('install', 'init', 'inspect', 'serve', 'run')) {
        $plain = Join-Path $Workdir "$log.log"
        $jsonl = Join-Path $Workdir "$log.jsonl"
        if (Test-Path -LiteralPath $plain -PathType Leaf) {
            Write-BoundedTail $plain (Join-Path $target "$log.log")
        } elseif (Test-Path -LiteralPath $jsonl -PathType Leaf) {
            Write-BoundedTail $jsonl (Join-Path $target "$log.jsonl")
        }
    }
    if ($Workspace -and (Test-Path -LiteralPath $Workspace -PathType Container)) {
        foreach ($path in @('putnami.workspace.json', 'putnami.lock.json', 'package.json', 'bun.lock', '.gitattributes', 'webapp/putnami.json', 'webapp/package.json')) {
            $source = Join-Path $Workspace $path
            if (Test-Path -LiteralPath $source -PathType Leaf) {
                Write-BoundedHead $source (Join-Path $target ($path -replace '/', '_')) 400
            }
        }
        # .npmrc is deliberately represented only by the bounded file list below:
        # even a rejected auth directive must never be copied into an artifact.
        $listing = Join-Path $Workdir 'workspace-files.all'
        $root = (Get-FullPath $Workspace).TrimEnd('\')
        $files = New-Object System.Collections.Generic.List[string]
        $pending = New-Object System.Collections.Generic.Stack[string]
        $pending.Push($root)
        while ($pending.Count -gt 0) {
            $directory = $pending.Pop()
            foreach ($entry in [IO.Directory]::GetFileSystemEntries($directory)) {
                $name = [IO.Path]::GetFileName($entry)
                if ([IO.Directory]::Exists($entry)) {
                    if (@('node_modules', '.putnami', '.git') -notcontains $name) { $pending.Push($entry) }
                } else {
                    $files.Add($entry)
                }
            }
        }
        $sorted = @($files | Sort-Object)
        [IO.File]::WriteAllText($listing, (($sorted -join "`n") + "`n"))
        Write-BoundedHead $listing (Join-Path $target 'workspace-files.txt') 400
    }
    Write-Smoke "smoke: bounded failure diagnostics written to $target"
}

# Stops the served starter the way Ctrl+C does and reports whether it stopped
# within the budget. The starter's graceful shutdown budget is 10s; the CLI
# gets enough headroom to cancel and end its job processes before the hard
# fallback.
function Stop-Server {
    if ($null -eq $script:Server) { return $true }
    $server = $script:Server
    $forced = $false
    try {
        if (-not $server.HasExited) {
            $breakError = $server.SendCtrlBreak()
            if ($breakError -ne 0) {
                Write-Smoke "smoke: CTRL_BREAK_EVENT could not reach process group $($server.Id) (Win32 error $breakError)"
            }
            if (-not $server.WaitForExit(20000)) {
                $forced = $true
                $server.Kill()
                [void]$server.WaitForExit(5000)
            }
        }
    } finally {
        $server.Dispose()
        $script:Server = $null
    }
    return (-not $forced)
}

function Remove-Workdir {
    if (-not $Workdir -or -not (Test-Path -LiteralPath $Workdir)) { return $true }
    for ($attempt = 1; $attempt -le 20; $attempt++) {
        try {
            Remove-Item -LiteralPath $Workdir -Recurse -Force -ErrorAction Stop
            return $true
        } catch {
            $script:CleanupError = $_.Exception.Message
            Start-Sleep -Milliseconds 500
        }
    }
    return $false
}

# Processes that still run an executable from the smoke workdir after the CLI
# stopped. On Windows, every process the CLI starts ends with it (ADR 0052).
function Get-OrphanedProcesses {
    $prefix = (Get-FullPath $Workdir).TrimEnd('\') + '\'
    return @(Get-CimInstance -ClassName Win32_Process | Where-Object {
            $_.ExecutablePath -and ([string]$_.ExecutablePath).StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)
        })
}

function Test-Integer([string]$Value) { return $Value -match '^[0-9]+$' }

function Invoke-Smoke {
    # -- Platform leg: this runner is the one row it proves. --
    $raw = [string]$env:PROCESSOR_ARCHITEW6432
    if (-not $raw) { $raw = [string]$env:PROCESSOR_ARCHITECTURE }
    if (([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) -or ($raw -ne 'AMD64')) {
        Stop-Smoke "platform leg: unsupported smoke runner $([Environment]::OSVersion.Platform)/$raw; this port proves windows/amd64, and smoke-check-release.sh proves darwin|linux x amd64|arm64"
    }

    $url = "$Base/putnami/cli/download?channel=$([Uri]::EscapeDataString($Channel))&os=windows&arch=amd64"

    # Establish the neutral machine before the first network request, so the
    # channel preflight and the installer run under the same environment.
    $script:Workspace = Join-Path $Workdir 'workspace'
    $smokeHome = Join-Path $Workdir 'home'
    foreach ($directory in @('workspace', 'home', 'store', 'artifacts', 'config', 'cache', 'data', 'state', 'config\appdata', 'data\localappdata')) {
        $null = [IO.Directory]::CreateDirectory((Join-Path $Workdir $directory))
    }
    $emptyGitConfig = Join-Path $Workdir 'config\gitconfig'
    $emptyNpmConfig = Join-Path $Workdir 'config\npmrc'
    [IO.File]::WriteAllText($emptyGitConfig, '')
    [IO.File]::WriteAllText($emptyNpmConfig, '')
    Set-Location -LiteralPath $Workspace

    # One home rule on Windows (D-W7): the CLI reads USERPROFILE. Git for
    # Windows and Bun also read HOME. %USERPROFILE%\.putnami\bin is deliberately
    # absent: the installer must create it.
    Set-SmokeEnv 'USERPROFILE' $smokeHome
    Set-SmokeEnv 'HOME' $smokeHome
    Set-SmokeEnv 'APPDATA' (Join-Path $Workdir 'config\appdata')
    Set-SmokeEnv 'LOCALAPPDATA' (Join-Path $Workdir 'data\localappdata')
    Set-SmokeEnv 'CURL_HOME' (Join-Path $Workdir 'config\curl')
    Set-SmokeEnv 'XDG_CONFIG_HOME' (Join-Path $Workdir 'config')
    Set-SmokeEnv 'XDG_CACHE_HOME' (Join-Path $Workdir 'cache')
    Set-SmokeEnv 'XDG_DATA_HOME' (Join-Path $Workdir 'data')
    Set-SmokeEnv 'XDG_STATE_HOME' (Join-Path $Workdir 'state')
    # SHELL means nothing to a Windows program; Git's sh reads BASH_ENV and ENV.
    Set-SmokeEnv 'SHELL' ''
    Set-SmokeEnv 'GIT_CONFIG_GLOBAL' $emptyGitConfig
    Set-SmokeEnv 'GIT_CONFIG_NOSYSTEM' '1'
    # Git's long-path support is a documented prerequisite (D-W8). With the
    # system and global configuration ignored, it is set here, and nothing else.
    Set-SmokeEnv 'GIT_CONFIG_COUNT' '1'
    Set-SmokeEnv 'GIT_CONFIG_KEY_0' 'core.longpaths'
    Set-SmokeEnv 'GIT_CONFIG_VALUE_0' 'true'
    Set-SmokeEnv 'GIT_CONFIG_PARAMETERS' ''
    # Package-manager routing is rebuilt from scratch below. In particular, an
    # ambient registry override must not redirect the starter's dependency
    # install. Windows environment names ignore case, so npm_config_* goes too.
    foreach ($name in @(Get-ChildItem Env: | ForEach-Object { $_.Name })) {
        if ($name -match '^(BUN_|NPM_CONFIG_)') { Set-SmokeEnv $name '' }
    }
    Set-SmokeEnv 'NPM_CONFIG_USERCONFIG' $emptyNpmConfig
    Set-SmokeEnv 'NPM_CONFIG_CACHE' (Join-Path $Workdir 'cache\npm')
    Set-SmokeEnv 'DOCKER_CONFIG' (Join-Path $Workdir 'config\docker')
    Set-SmokeEnv 'BUN_INSTALL' (Join-Path $Workdir 'data\bun')
    Set-SmokeEnv 'BUN_INSTALL_CACHE_DIR' (Join-Path $Workdir 'cache\bun')
    # Fail closed against both current and future Putnami overrides. The
    # harness captured its channel and registry inputs above; everything under
    # this prefix is now rebuilt from the neutral workdir or an explicit
    # disabled value.
    foreach ($name in @(Get-ChildItem Env: | ForEach-Object { $_.Name })) {
        if ($name -match '^PUTNAMI_') { Set-SmokeEnv $name '' }
    }
    Set-SmokeEnv 'PUTNAMI_BUN_CACHE_DIR' (Join-Path $Workdir 'cache\putnami-bun')
    Set-SmokeEnv 'PUTNAMI_STORE_DIR' (Join-Path $Workdir 'store')
    Set-SmokeEnv 'PUTNAMI_ARTIFACT_DIR' (Join-Path $Workdir 'artifacts')
    Set-SmokeEnv 'PUTNAMI_NO_RELAUNCH' '1'
    Set-SmokeEnv 'PUTNAMI_REGISTRY_URL' $Base
    Set-SmokeEnv 'PUTNAMI_VERSION' $Channel
    Set-SmokeEnv 'PUTNAMI_TELEMETRY' 'off'
    Set-SmokeEnv 'DO_NOT_TRACK' '1'
    foreach ($name in @(
            'PUTNAMI_CACHE_URL', 'PUTNAMI_CACHE_TOKEN', 'PUTNAMI_CLOUD_TOKEN', 'PUTNAMI_TELEMETRY_ENDPOINT',
            'CONFIG_SERVER_URL', 'CONFIG_SERVER_TOKEN', 'PUTNAMI_TOKEN', 'GOOGLE_APPLICATION_CREDENTIALS',
            'AWS_ACCESS_KEY_ID', 'AWS_SECRET_ACCESS_KEY', 'AWS_SESSION_TOKEN', 'AWS_PROFILE', 'AWS_CONFIG_FILE',
            'AWS_SHARED_CREDENTIALS_FILE', 'GITHUB_TOKEN', 'GH_TOKEN', 'NPM_TOKEN', 'NODE_AUTH_TOKEN',
            'BUN_AUTH_TOKEN', 'SSH_AUTH_SOCK', 'GIT_ASKPASS', 'SSH_ASKPASS', 'BASH_ENV', 'ENV',
            'HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY', 'NO_PROXY')) {
        Set-SmokeEnv $name ''
    }

    # -- Prerequisites leg --
    # The TypeScript extension installs dependencies and serves the starter
    # through Bun. Name that prerequisite, and the Windows ones, before touching
    # the release channel instead of turning a missing one into a much later,
    # less useful init failure.
    $bun = Get-Command -Name 'bun' -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -eq $bun) {
        Stop-Smoke 'prerequisites leg: Bun v1.4.0 or later is required for the TypeScript web init/serve golden path'
    }
    $bunPath = [string]$bun.Path
    $bunLog = Join-Path $Workdir 'bun-version.log'
    [void](Invoke-SmokeCommand $bunPath 'bun --version' $Workdir $bunLog)
    $bunVersion = ''
    $bunLines = @(Get-LogTail $bunLog 200)
    if ($bunLines.Count -gt 0) { $bunVersion = ([string]$bunLines[0]).Trim() }
    $bunCore = ($bunVersion -split '[-+]')[0]
    if ($bunCore -notmatch '^([0-9]+)(\.([0-9]+))?(\.([0-9]+))?$') {
        $shown = $bunVersion
        if (-not $shown) { $shown = '<none>' }
        Stop-Smoke "prerequisites leg: $bunPath reports unsupported version '$shown'; Bun v1.4.0 or later is required"
    }
    $bunMajor = [int]$Matches[1]
    $bunMinor = 0
    if ($Matches[3]) { $bunMinor = [int]$Matches[3] }
    if (($bunMajor -lt 1) -or (($bunMajor -eq 1) -and ($bunMinor -lt 4))) {
        Stop-Smoke "prerequisites leg: $bunPath reports unsupported version '$bunVersion'; Bun v1.4.0 or later is required"
    }
    Write-Smoke "smoke: prerequisites include Bun $bunVersion at $bunPath"
    # Workspace hooks run through sh -c, from Git for Windows (D-W3).
    foreach ($tool in @('git', 'sh')) {
        if ($null -eq (Get-Command -Name $tool -CommandType Application -ErrorAction SilentlyContinue)) {
            Stop-Smoke "prerequisites leg: '$tool' is not on PATH; install Git for Windows, which provides git and sh"
        }
    }
    # Store paths exceed 260 characters (D-W8).
    $longPaths = $null
    $fileSystem = [Microsoft.Win32.Registry]::LocalMachine.OpenSubKey('SYSTEM\CurrentControlSet\Control\FileSystem', $false)
    if ($null -ne $fileSystem) {
        try { $longPaths = $fileSystem.GetValue('LongPathsEnabled', $null) } finally { $fileSystem.Close() }
    }
    if ($longPaths -ne 1) {
        Stop-Smoke 'prerequisites leg: LongPathsEnabled is off; enable it as an administrator with: New-ItemProperty -Path HKLM:\SYSTEM\CurrentControlSet\Control\FileSystem -Name LongPathsEnabled -Value 1 -PropertyType DWORD -Force'
    }
    if (-not (Test-Path -LiteralPath (Join-Path $env:SystemRoot 'System32\tar.exe') -PathType Leaf)) {
        Stop-Smoke 'prerequisites leg: tar.exe is missing from System32; Windows 10 version 1803 or later ships it'
    }
    Write-Smoke "smoke: GET $url"

    # -- Leg 1: the channel resolves, and it advertises what the installer needs --
    #
    # The installer is fail-closed, so a release that stops advertising a digest
    # would break every new install. That is a release failure, not an install
    # failure, and it is cheaper to name here than to read out of an install
    # log. A freshly-published artifact may take a moment to propagate; retry
    # transient non-200s before failing the release.
    #
    # Windows PowerShell 5.1 can still offer TLS 1.0 by default.
    $protocols = [Net.ServicePointManager]::SecurityProtocol
    if ([int]$protocols -ne 0) {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType](3072 -bor ([int]$protocols -band 12288))
    }
    $asset = Join-Path $Workdir 'asset'
    $status = 0
    $headers = @{}
    for ($attempt = 1; $attempt -le $Retries; $attempt++) {
        $response = Invoke-SmokeGet $url $asset 120000
        $status = $response.Status
        $headers = $response.Headers
        if ($status -eq 200) { break }
        if ($attempt -ge $Retries) { break }
        Write-Smoke "smoke: attempt $attempt/$Retries got HTTP $status; retrying in 10s..."
        Start-Sleep -Seconds 10
    }
    if ($status -ne 200) {
        Stop-Smoke "channel leg: CLI download channel '$Channel' returned HTTP $status for windows/amd64 - the CLI binary was not published"
    }
    $resolved = ''
    if ($headers.ContainsKey('x-resolved-version')) { $resolved = ([string]$headers['x-resolved-version']).Trim() }
    $advertised = ''
    if ($headers.ContainsKey('x-integrity')) { $advertised = ([string]$headers['x-integrity']).Trim() }
    if (-not $advertised -and $headers.ContainsKey('digest')) {
        if ([string]$headers['digest'] -match '(?i)sha-256=([^,]*)') { $advertised = $Matches[1].Trim() }
    }
    if (-not $resolved) {
        Stop-Smoke "channel leg: CLI download channel '$Channel' returned no X-Resolved-Version header"
    }
    if (-not $advertised) {
        Stop-Smoke "channel leg: CLI download channel '$Channel' advertised no SHA-256 (X-Integrity or RFC 9530 Digest) - the fail-closed installer refuses this release"
    }

    # The registry must agree with itself. If the advertised digest does not
    # describe the bytes it just streamed, every install fails the integrity
    # check; naming it here costs one hash and saves reading an install log.
    $actual = Get-Sha256 $asset
    $expected = ($advertised -replace '^(?i)sha-?256[:-]', '').ToLowerInvariant()
    if ($actual -cne $expected) {
        Stop-Smoke "channel leg: channel '$Channel' advertises $expected but streamed bytes hashing to $actual - the fail-closed installer refuses this release"
    }
    $channelExecutable = Get-ChannelExecutable $asset
    if (-not $channelExecutable) {
        Stop-Smoke "channel leg: channel '$Channel' served neither a .tar.gz with compiled\putnami.exe nor a Windows executable"
    }
    $channelDigest = Get-Sha256 $channelExecutable
    Write-Smoke "smoke: channel '$Channel' resolved $resolved, advertised $advertised, bytes match"

    # -- Leg 2: install with the published installer --
    #
    # A one-line installer that asks for elevation is one a user cannot audit
    # before it runs, so the install leg runs with sudo, gsudo and runas on PATH
    # that record the attempt and fail. The marker must not exist afterwards.
    $fakeBin = Join-Path $Workdir 'fakebin'
    $sudoMarker = Join-Path $Workdir 'sudo-was-invoked'
    $null = [IO.Directory]::CreateDirectory($fakeBin)
    foreach ($escalator in @('sudo', 'gsudo', 'runas')) {
        [IO.File]::WriteAllText((Join-Path $fakeBin "$escalator.cmd"), "@echo off`r`n>>""$sudoMarker"" echo $escalator %*`r`nexit /b 1`r`n")
    }
    $installLog = Join-Path $Workdir 'install.log'
    $discovered = Join-Path $Workdir 'discovered.txt'
    $script:SavedUserPath = Get-UserPathValue
    # The session that ran the installer records what it resolves 'putnami' to.
    $discovery = '$c = Get-Command -Name putnami -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1; ' +
    'if ($c) { [IO.File]::WriteAllText(''' + ($discovered -replace "'", "''") + ''', $c.Path) }'
    if ($InstallerUrl -eq $PublicInstallerUrl) {
        $oneLiner = 'irm https://putnami.dev/install.ps1 | iex'
        $installFailure = "install leg: public installer failed against channel '$Channel'"
    } else {
        $oneLiner = "irm $InstallerUrl | iex"
        $installFailure = "install leg: $InstallerUrl failed against channel '$Channel'"
    }
    # The one-liner runs as PowerShell script text, the way a user types it and
    # the way the install guide gives it, in a script started with -File.
    # Microsoft Defender blocks the one-liner when a command line passes it with
    # -Command (tooling/cli/doc/22-installing-the-cli.md).
    Write-Smoke "smoke: $oneLiner"
    $installScript = Join-Path $Workdir 'install-leg.ps1'
    [IO.File]::WriteAllText($installScript, $oneLiner + "`r`n" + $discovery + "`r`n", [Text.Encoding]::ASCII)
    $installCommand = 'powershell -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "' + $installScript + '"'
    $pathBeforeInstall = $env:PATH
    Set-SmokeEnv 'PATH' "$fakeBin;$pathBeforeInstall"
    try {
        $installStatus = Invoke-SmokeCommand (Get-SmokePowerShell) $installCommand $Workspace $installLog
    } finally {
        Set-SmokeEnv 'PATH' $pathBeforeInstall
    }
    if ($installStatus -ne 0) { Stop-Smoke $installFailure $installLog }
    if (Test-Path -LiteralPath $sudoMarker) {
        Write-Smoke '::error::install leg: the installer escalated privileges - a one-line install must never ask for elevation'
        Write-LogTail $sudoMarker 200
        throw $SmokeFailed
    }

    # The installer prints "Integrity verified" only after comparing the
    # download to the digest the registry advertised. A regression that drops
    # the comparison, or silently takes the PUTNAMI_UNSAFE_INSTALL escape hatch,
    # fails here.
    $installText = Read-SharedText $installLog
    if (-not $installText.Contains('Integrity verified')) {
        Stop-Smoke 'install leg: the installer did not report a verified digest - the public install path may no longer verify what it downloads' $installLog
    }
    if ($installText.Contains('without integrity verification')) {
        Stop-Smoke 'install leg: the installer took the unverified path (PUTNAMI_UNSAFE_INSTALL) - a release must never need it' $installLog
    }
    Write-Smoke 'smoke: installer verified the download against the advertised digest'

    # -- Leg 3: discovery, in the session that ran the installer and in a new terminal --
    $bin = ''
    if (Test-Path -LiteralPath $discovered -PathType Leaf) { $bin = ([IO.File]::ReadAllText($discovered)).Trim() }
    if (-not $bin) {
        Stop-Smoke "discovery leg: 'putnami' is not runnable in the session that ran the installer" $installLog
    }
    if (-not (Test-PathInside $bin $Workdir)) {
        Stop-Smoke "discovery leg: 'putnami' resolved to $bin, outside the smoke workdir - the smoke would be testing an ambient install"
    }
    Write-Smoke "smoke: discovered $bin"
    $user = Get-UserPathValue
    $userPath = ''
    if ($null -ne $user.Value) { $userPath = $user.Value }
    $newTerminal = Find-CommandInPathList ((Get-MachinePathValue) + ';' + $userPath) 'putnami'
    if (-not $newTerminal) {
        Stop-Smoke "discovery leg: a new terminal finds no 'putnami' through the machine and user Path in the registry" $installLog
    }
    if ((Get-FullPath $newTerminal) -ne (Get-FullPath $bin)) {
        Stop-Smoke "discovery leg: a new terminal resolves 'putnami' to $newTerminal, not to the installed $bin"
    }
    Write-Smoke "smoke: a new terminal resolves putnami to $newTerminal"
    if ([IO.Path]::GetExtension($bin) -ne '.exe') {
        Stop-Smoke "platform leg: installed $bin is not a Windows executable"
    }

    # The independently fetched candidate bytes are for this host target.
    # Requiring the installed executable to have the same digest proves the
    # installer selected that target rather than an ambient or
    # wrong-architecture artifact; running --version below proves the selected
    # bytes execute here.
    $installedDigest = Get-Sha256 $bin
    if ($installedDigest -cne $channelDigest) {
        Stop-Smoke "platform leg: installed windows/amd64 executable hashes to $installedDigest, candidate channel executable hashes to $channelDigest"
    }
    Write-Smoke "smoke: selected executable windows/amd64, digest $installedDigest"
    Set-SmokeEnv 'PATH' ((Split-Path -Parent $bin) + ';' + $env:PATH)

    # -- Leg 4: the installed binary is the version the channel resolved --
    $versionLog = Join-Path $Workdir 'version.log'
    [void](Invoke-SmokeCommand $bin 'putnami --version' $Workspace $versionLog)
    $reported = ''
    $versionLines = @(Get-LogTail $versionLog 100000)
    if ($versionLines.Count -gt 0) {
        $fields = @(([string]$versionLines[0]).Trim() -split '\s+')
        $reported = $fields[$fields.Count - 1]
    }
    $shownVersion = $reported
    if (-not $shownVersion) { $shownVersion = '<none>' }
    Write-Smoke "smoke: installed binary reports $shownVersion"
    if (($reported -creplace '^v', '') -cne ($resolved -creplace '^v', '')) {
        Stop-Smoke "stamp leg: channel '$Channel' resolves $resolved but the installed binary reports $shownVersion"
    }

    # -- Leg 5: the exact public TypeScript starter initializes --
    $initLog = Join-Path $Workdir 'init.log'
    Write-Smoke 'smoke: putnami init --project webapp --extension ts'
    if ((Invoke-SmokeCommand $bin 'putnami init --project webapp --extension ts' $Workspace $initLog) -ne 0) {
        Stop-Smoke 'init leg: published CLI failed the exact public TypeScript init command' $initLog
    }
    foreach ($path in @('putnami.workspace.json', 'putnami.lock.json', 'package.json', 'bun.lock', '.npmrc',
            'CLAUDE.md', 'AGENTS.md', '.mcp.json', '.gitattributes', 'webapp/putnami.json', 'webapp/package.json')) {
        $file = Join-Path $Workspace $path
        if (-not ((Test-Path -LiteralPath $file -PathType Leaf) -and ((Get-Item -LiteralPath $file).Length -gt 0))) {
            Stop-Smoke "init leg: generated workspace is missing non-empty $path" $initLog
        }
    }
    foreach ($required in @(
            @('putnami.workspace.json', '"@putnami/typescript"'),
            @('putnami.workspace.json', '"typescript-web"'),
            @('putnami.workspace.json', '"webapp"'),
            @('putnami.lock.json', '"@putnami/typescript"'),
            @('putnami.lock.json', '"typescript-web"'),
            @('webapp/putnami.json', '"name": "webapp"'),
            @('webapp/putnami.json', '"@putnami/typescript"'),
            @('AGENTS.md', 'This is a Putnami workspace.'),
            @('.mcp.json', '"putnami"'),
            @('.gitattributes', '* text=auto eol=lf'))) {
        $path = $required[0]
        $text = $required[1]
        if (-not (Read-SharedText (Join-Path $Workspace $path)).Contains($text)) {
            Write-Smoke "::error::init leg: generated $path does not contain required state: $text"
            foreach ($line in @(Get-TextLines (Read-SharedText (Join-Path $Workspace $path))) | Select-Object -First 200) { Write-Smoke $line }
            throw $SmokeFailed
        }
    }
    $npmrc = Read-SharedText (Join-Path $Workspace '.npmrc')
    foreach ($line in (Get-TextLines $npmrc)) {
        $key = ($line -split '=', 2)[0]
        if ($key.ToLowerInvariant() -match '(_authtoken|_auth)\s*$') {
            $authKey = ($key -replace '^.*:', '') -replace '\s', ''
            Stop-Smoke "init leg: generated .npmrc contains authentication directive $authKey; the public starter must install without credentials"
        }
    }
    if (-not $npmrc.Contains('@putnami:registry=https://npm.putnami.dev')) {
        Stop-Smoke 'init leg: generated .npmrc does not contain required state: @putnami:registry=https://npm.putnami.dev'
    }
    foreach ($path in @('putnami.workspace.json', 'putnami.lock.json', 'package.json', 'bun.lock', '.npmrc', 'webapp/putnami.json', 'webapp/package.json')) {
        if ((Read-SharedText (Join-Path $Workspace $path)).Contains('@putnami/cloud')) {
            Stop-Smoke 'init leg: public starter unexpectedly depends on private @putnami/cloud state'
        }
    }

    $inspectLog = Join-Path $Workdir 'inspect.jsonl'
    foreach ($inspect in @(
            'putnami workspace describe --output=jsonl',
            'putnami projects describe webapp --output=jsonl',
            'putnami extensions list --output=jsonl',
            'putnami build --projects webapp --plan --output=jsonl')) {
        if ((Invoke-SmokeCommand $bin $inspect $Workspace $inspectLog) -ne 0) {
            Stop-Smoke 'init leg: generated workspace/project/extension state did not validate through the installed CLI' $inspectLog
        }
    }
    if (-not (Read-SharedText $inspectLog).Contains('@putnami/typescript')) {
        Stop-Smoke 'init leg: machine-readable inspection did not report @putnami/typescript' $inspectLog
    }
    Write-Smoke 'smoke: generated workspace, webapp, locks, extension and MCP registration verified; build plan validates'

    # -- Leg 6: serve, typed readiness, a real HTTP response, a clean stop --
    #
    # putnami serve runs as the root of its own console process group, as the
    # foreground program of a terminal is the one Ctrl+C reaches. The CLI gets
    # the CTRL_BREAK_EVENT the shutdown leg sends, and Go reads it as the
    # interrupt Ctrl+C is.
    $serveLog = Join-Path $Workdir 'serve.jsonl'
    [IO.File]::WriteAllText($serveLog, '')
    Write-Smoke 'smoke: putnami serve webapp'
    Set-SmokeEnv 'PORT' '0'
    Set-SmokeEnv 'PUTNAMI_OUTPUT' 'jsonl'
    try {
        $script:Server = [PutnamiSmoke.ChildProcess]::Start($bin, (Get-CommandLine $bin 'putnami serve webapp'), $Workspace, $serveLog, $true)
    } finally {
        Set-SmokeEnv 'PORT' ''
        Set-SmokeEnv 'PUTNAMI_OUTPUT' ''
    }

    # The framework reports the listener's actual OS-assigned port through the
    # typed ready event. Waiting on that event avoids both fixed-port collisions
    # and a check-then-bind race, while the deadline keeps release failures
    # bounded.
    $readyPort = ''
    $deadline = [DateTime]::UtcNow.AddSeconds($StartupTimeout)
    while (-not $readyPort) {
        $serveText = Read-SharedText $serveLog
        foreach ($line in (Get-TextLines $serveText)) {
            if ($line -match '"type":"ready".*"port":([0-9]+)') { $readyPort = $Matches[1] }
        }
        if ($readyPort) { break }
        # serve watches by default and stays resident after a failed run so it
        # can retry on changes. A completed session before readiness is
        # nevertheless a terminal smoke failure.
        if ($serveText.Contains('"record":"session:end"')) {
            Stop-Smoke "serve leg: webapp's initial serve session ended before readiness" $serveLog
        }
        if ($script:Server.HasExited) {
            $serverStatus = $script:Server.ExitCode
            $script:Server.Dispose()
            $script:Server = $null
            Stop-Smoke "serve leg: webapp exited with status $serverStatus before readiness" $serveLog
        }
        if ([DateTime]::UtcNow -ge $deadline) {
            Stop-Smoke "serve leg: webapp did not emit readiness within ${StartupTimeout}s" $serveLog
        }
        Start-Sleep -Milliseconds 250
    }

    $responseFile = Join-Path $Workdir 'response.html'
    Write-Smoke "smoke: GET http://127.0.0.1:${readyPort}/"
    $page = Invoke-SmokeGet "http://127.0.0.1:${readyPort}/" $responseFile 10000
    if ($page.Status -eq 0) {
        Stop-Smoke "http leg: webapp did not answer after its ready event (HTTP 000: $($page.Error))" $serveLog
    }
    if (($page.Status -lt 200) -or ($page.Status -gt 299)) {
        Stop-Smoke "http leg: webapp returned HTTP $($page.Status); expected an explicit 2xx response" $serveLog
    }
    if (-not ((Test-Path -LiteralPath $responseFile -PathType Leaf) -and ((Get-Item -LiteralPath $responseFile).Length -gt 0))) {
        Stop-Smoke 'http leg: webapp returned an empty successful response' $serveLog
    }

    if (-not (Stop-Server)) {
        Stop-Smoke 'shutdown leg: webapp did not stop within the graceful shutdown budget' $serveLog
    }
    $listenerStopped = $false
    for ($probe = 0; $probe -lt 20; $probe++) {
        $answer = Invoke-SmokeGet "http://127.0.0.1:${readyPort}/" (Join-Path $Workdir 'probe.html') 1000
        if ($answer.Status -eq 0) {
            $listenerStopped = $true
            break
        }
        Start-Sleep -Milliseconds 100
    }
    if (-not $listenerStopped) {
        Stop-Smoke 'shutdown leg: HTTP listener still answers after the CLI exited; the starter may be orphaned' $serveLog
    }
    Write-Smoke 'smoke: HTTP listener is unreachable after clean stop'

    # -- Optional leg: run one command without a workspace --
    #
    # The run form installs the CLI, pins the extension the command map names
    # for the user, and runs the command in the caller's directory, which it
    # must never write to. The caller here is a fresh `git init` directory
    # outside the starter workspace: git's view of it (changes, untracked and
    # ignored files) and its listing (git does not report an empty directory)
    # must both be unchanged.
    if ($RunCommand) {
        $runDir = Join-Path $Workdir 'run-scenario'
        $runLog = Join-Path $Workdir 'run.log'
        $null = [IO.Directory]::CreateDirectory($runDir)
        $git = [string](Get-Command -Name git -CommandType Application | Select-Object -First 1).Path
        if ((Invoke-SmokeCommand $git 'git init -q' $runDir $runLog) -ne 0) {
            Stop-Smoke "run leg: git init failed in $runDir" $runLog
        }
        $runMap = ''
        if ($InstallerUrl -eq $PublicInstallerUrl) {
            $runLine = 'irm "https://putnami.dev/install.ps1?run=' + $RunCommand + '" | iex'
        } else {
            # The installer reads the command map published next to it; an
            # installer served from elsewhere is paired with the map served
            # beside it. Single quotes keep a $ in the URL from expanding.
            $runLine = "irm '" + $InstallerUrl + '?run=' + $RunCommand + "' | iex"
            $runMap = $InstallerUrl.Substring(0, $InstallerUrl.LastIndexOf('/')) + '/install-commands.txt'
        }
        Write-Smoke "smoke: $runLine"
        # As in the install leg, the one-liner runs as script text in a script
        # started with -File. From text the installer leaves the command's exit
        # code in $LASTEXITCODE, and the script ends with it.
        $runScript = Join-Path $Workdir 'run-leg.ps1'
        [IO.File]::WriteAllText($runScript, $runLine + "`r`n" + 'exit $LASTEXITCODE' + "`r`n", [Text.Encoding]::ASCII)
        $runCommandLine = 'powershell -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "' + $runScript + '"'
        $pathBeforeRun = $env:PATH
        Set-SmokeEnv 'PATH' "$fakeBin;$pathBeforeRun"
        if ($runMap) { Set-SmokeEnv 'PUTNAMI_COMMAND_MAP_URL' $runMap }
        try {
            $runStatus = Invoke-SmokeCommand (Get-SmokePowerShell) $runCommandLine $runDir $runLog
        } finally {
            Set-SmokeEnv 'PATH' $pathBeforeRun
            Set-SmokeEnv 'PUTNAMI_COMMAND_MAP_URL' ''
        }
        if ($runStatus -ne 0) {
            Stop-Smoke "run leg: putnami $RunCommand through ${InstallerUrl}?run=$RunCommand exited $runStatus" $runLog
        }
        if (Test-Path -LiteralPath $sudoMarker) {
            Write-Smoke '::error::run leg: the installer escalated privileges - a one-line install must never ask for elevation'
            Write-LogTail $sudoMarker 200
            throw $SmokeFailed
        }
        $runStatusLog = Join-Path $Workdir 'run-status.log'
        if ((Invoke-SmokeCommand $git 'git status --porcelain --untracked-files=all --ignored' $runDir $runStatusLog) -ne 0) {
            Stop-Smoke "run leg: git status failed in $runDir" $runStatusLog
        }
        $runChanges = @(Get-LogTail $runStatusLog 50)
        $runEntries = @([IO.Directory]::GetFileSystemEntries($runDir) | ForEach-Object { [IO.Path]::GetFileName($_) } | Sort-Object)
        if (($runChanges.Count -gt 0) -or (($runEntries -join ',') -ne '.git')) {
            Write-Smoke "::error::run leg: putnami $RunCommand changed the directory it ran in; the run form must leave the caller's directory untouched"
            foreach ($line in $runChanges) { Write-Smoke $line }
            foreach ($entry in @($runEntries | Select-Object -First 50)) { Write-Smoke $entry }
            Write-LogTail $runLog 200
            throw $SmokeFailed
        }
        Write-Smoke "smoke: putnami $RunCommand exited 0 and left $runDir untouched"
        Write-Smoke "smoke: OK - channel '$Channel' passes irm install -> TypeScript init -> serve -> HTTP -> clean stop -> run $RunCommand on windows/amd64"
        return
    }
    Write-Smoke "smoke: OK - channel '$Channel' passes irm install -> TypeScript init -> serve -> HTTP -> clean stop on windows/amd64"
}

# -- Inputs --
$Base = [string]$env:PUTNAMI_REGISTRY_URL
if (-not $Base) { $Base = 'https://put.putnami.dev' }
$Base = $Base.TrimEnd('/')
$RetriesText = [string]$env:SMOKE_RETRIES
if (-not $RetriesText) { $RetriesText = '5' }
$StartupTimeoutText = [string]$env:SMOKE_STARTUP_TIMEOUT
if (-not $StartupTimeoutText) { $StartupTimeoutText = '180' }
$InstallerUrl = [string]$env:SMOKE_INSTALL_URL
if (-not $InstallerUrl) { $InstallerUrl = $PublicInstallerUrl }
$RunCommand = [string]$env:SMOKE_RUN_COMMAND
$DiagnosticsDir = [string]$env:SMOKE_DIAGNOSTICS_DIR
if ($DiagnosticsDir -and -not [IO.Path]::IsPathRooted($DiagnosticsDir)) {
    $DiagnosticsDir = Join-Path $LaunchDir $DiagnosticsDir
}

if (-not (Test-Integer $StartupTimeoutText)) {
    Write-Smoke "::error::SMOKE_STARTUP_TIMEOUT must be a non-negative integer, got '$StartupTimeoutText'"
    exit 1
}
if (-not (Test-Integer $RetriesText) -or ([long]$RetriesText -lt 1)) {
    Write-Smoke "::error::SMOKE_RETRIES must be a positive integer, got '$RetriesText'"
    exit 1
}
# The URL becomes part of a PowerShell command line.
if ($InstallerUrl -notmatch '^https?://[^\s"''`;|&<>]+$') {
    Write-Smoke "::error::SMOKE_INSTALL_URL must be an http(s) URL without spaces, quotes or shell operators, got '$InstallerUrl'"
    exit 1
}
# The installer and the site accept the same command names. -cmatch compares
# code points, so a-z is exactly the 26 lowercase ASCII letters, and \z anchors
# at the very end, where $ also matches before a final newline.
if ($RunCommand -and ($RunCommand -cnotmatch '^[a-z][a-z0-9-]{0,63}\z')) {
    Write-Smoke "::error::SMOKE_RUN_COMMAND must match ^[a-z][a-z0-9-]{0,63}`$, got '$RunCommand'"
    exit 1
}
$Retries = [int]$RetriesText
$StartupTimeout = [int]$StartupTimeoutText

if (-not ('PutnamiSmoke.ChildProcess' -as [type])) {
    Add-Type -TypeDefinition $NativeSource -Language CSharp
}

$Workdir = Join-Path ([IO.Path]::GetTempPath()) ('putnami-smoke-' + [Guid]::NewGuid().ToString('N').Substring(0, 12))
$null = [IO.Directory]::CreateDirectory($Workdir)
$Workdir = Get-FullPath $Workdir
$Workspace = ''
$Server = $null
$SavedUserPath = $null
$CleanupError = ''
$SmokeStatus = 1
try {
    Invoke-Smoke
    $SmokeStatus = 0
} catch {
    if ([string]$_.Exception.Message -ne $SmokeFailed) {
        Write-Smoke "::error::smoke: unexpected failure: $($_.Exception.Message)"
        Write-Smoke ([string]$_.InvocationInfo.PositionMessage)
    }
    $SmokeStatus = 1
} finally {
    try { [void](Stop-Server) } catch { }
    if ($SmokeStatus -ne 0) {
        try { Save-FailureArtifacts } catch { Write-Smoke "smoke: could not write the failure diagnostics: $($_.Exception.Message)" }
    }
    try { Restore-UserPath } catch { Write-Smoke "::error::cleanup leg: could not restore the user Path in HKCU\Environment: $($_.Exception.Message)"; $SmokeStatus = 1 }
    Set-Location -LiteralPath $LaunchDir
    if (-not (Remove-Workdir)) {
        $orphans = @(Get-OrphanedProcesses)
        foreach ($orphan in $orphans) {
            Write-Smoke "smoke: still running from the smoke workdir: pid $($orphan.ProcessId) $($orphan.CommandLine)"
            Stop-Process -Id $orphan.ProcessId -Force -ErrorAction SilentlyContinue
        }
        if ($orphans.Count -gt 0) {
            Write-Smoke "::error::cleanup leg: $($orphans.Count) process(es) started by the smoke outlived the CLI and held ${Workdir}"
            [void](Remove-Workdir)
        } else {
            Write-Smoke "::error::cleanup leg: cannot remove the smoke workdir ${Workdir}: $CleanupError"
        }
        $SmokeStatus = 1
    }
}
exit $SmokeStatus
