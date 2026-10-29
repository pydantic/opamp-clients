# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
#
# pydantic-opamp-enroll (Windows): enrolls this host with an OpAMP server in one step. As an
# administrator:
#
#   $env:ENROLLMENT_TOKEN | & "$env:ProgramFiles\pydantic-opamp\pydantic-opamp-enroll.ps1" -Endpoint wss://opamp.example.com/v1/opamp
#
# It writes the enrollment token to a file only SYSTEM and Administrators can read (never on the
# command line), writes the Supervisor's config, and starts the service. The Supervisor seals its
# state with a key protected by DPAPI for SYSTEM; the server's issued credentials then replace
# the enrollment token, kept sealed in ProgramData\pydantic-opamp\var.
#
# OPAMP_ENROLL_ROOT is for its tests: the directory to write in place of ProgramData\pydantic-opamp.
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Endpoint,
    # Where to read the enrollment token; standard input by default.
    [string]$TokenFile,
    # Read the enrollment token and telemetry token from two standard-input lines.
    [switch]$TelemetryTokenStdin,
    # A CA bundle to trust for the server, in place of the system's.
    [string]$CaFile,
    # Allow ws:// or http:// (the token then crosses the network in the clear).
    [switch]$Plaintext,
    # Limit what the server's configs may do: a few Logfire components, sending only to the
    # -Endpoint's host, no environment or file references. By default any config it sends runs.
    [switch]$Restricted,
    # With -Restricted, where the server's configs may send telemetry, as scheme://host[:port]:
    # the -Endpoint's host, over https, by default.
    [string[]]$ExporterEndpoint,
    # With -Restricted, the files, and directories, the server's configs may read logs from: none
    # by default.
    [string[]]$LogPath,
    # With -Restricted, let the server's configs read this host's Windows event logs (never a
    # remote host's), keeping their read positions in ProgramData\pydantic-opamp\var\collector.
    [switch]$HostLogs,
    # Install the collector the server offers, if this repository's release workflow signed it;
    # by default only the installed collector runs.
    [switch]$AcceptPackages,
    # Write the config and the token, but don't start the service.
    [switch]$NoStart,
    # Takes the token's lines when they're piped in, read below from $input. PowerShell pipes
    # standard input this way too (powershell -File): without it, they couldn't be bound.
    [Parameter(ValueFromPipeline = $true)]$PipedLine
)
$ErrorActionPreference = 'Stop'
$service = 'pydantic-opamp-supervisor'

function Fail([string]$message) {
    [Console]::Error.WriteLine("pydantic-opamp-enroll: $message")
    exit 1
}

if ($Endpoint -match '^(wss|https)://') {
} elseif ($Endpoint -match '^(ws|http)://') {
    if (-not $Plaintext) { Fail "$Endpoint isn't encrypted; pass -Plaintext to use it anyway" }
} else {
    Fail 'the endpoint must be a wss://, https://, ws:// or http:// URL'
}
if ($Endpoint -match "[`"'\\ ]") { Fail "the endpoint can't contain quotes, backslashes or spaces" }
if ($CaFile) {
    if (-not [System.IO.Path]::IsPathRooted($CaFile)) { Fail '-CaFile needs an absolute path' }
    if (-not (Test-Path -LiteralPath $CaFile -PathType Leaf)) { Fail "can't read $CaFile" }
    if ($CaFile -match "'") { Fail "-CaFile can't contain a single quote" }
}

$authority = ($Endpoint -replace '^[a-z]+://', '') -replace '[/?#].*$', ''
if ($authority.Contains('@')) { Fail "the endpoint is scheme://host[:port]/path, with nothing else: $Endpoint" }
# The policy's options only widen what -Restricted allows: without it, everything is allowed.
if (-not $Restricted -and ($ExporterEndpoint -or $LogPath -or $HostLogs)) {
    Fail '-ExporterEndpoint, -LogPath and -HostLogs widen a -Restricted policy; without -Restricted the server''s configs may already send anywhere and read any log'
}
# Where the remote config's exporters may send: the OpAMP server's host unless given.
if ($Restricted -and -not $ExporterEndpoint) {
    $ExporterEndpoint = @($(if ($Endpoint -match '^(wss|https)://') { "https://$authority" } else { "http://$authority" }))
}
foreach ($e in $ExporterEndpoint) {
    if ($e -match '^http://') {
        if (-not $Plaintext) { Fail "$e isn't encrypted; pass -Plaintext to use it anyway" }
    } elseif ($e -notmatch '^https://') {
        Fail "an exporter endpoint must be an https:// or http:// URL: $e"
    }
    $rest = ($e -replace '^https?://', '') -replace '/$', ''
    if (-not $rest -or $rest -match "[/?#@`"'\\ `$]") { Fail "an exporter endpoint is scheme://host[:port], with nothing else: $e" }
}
foreach ($p in $LogPath) {
    if ($p -notmatch '^[A-Za-z]:[\\/]') { Fail "-LogPath needs an absolute path, with its drive: $p" }
    if ($p -match "['`"*?\[\]{}`$]" -or $p.Contains('..') -or $p.Substring(2).Contains(':')) {
        Fail "-LogPath can't hold quotes, glob characters, `$, .. or a second colon: $p"
    }
}

$root = $env:OPAMP_ENROLL_ROOT
if (-not $root) {
    $admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
        [Security.Principal.WindowsBuiltInRole]::Administrator)
    if (-not $admin) { Fail 'run it as an administrator' }
    $root = Join-Path $env:ProgramData 'pydantic-opamp'
}

$token = if ($TokenFile) { Get-Content -LiteralPath $TokenFile -Raw }
         elseif ($MyInvocation.ExpectingInput) { @($input) -join "`n" }
         else { [Console]::In.ReadToEnd() }
$telemetryToken = $null
if ($TelemetryTokenStdin) {
    if ($TokenFile) { Fail '-TelemetryTokenStdin cannot be combined with -TokenFile' }
    $lines = @($token.TrimEnd([char[]]"`r`n") -split '\r?\n')
    if ($lines.Count -ne 2) { Fail 'provide exactly two credential lines' }
    $token, $telemetryToken = $lines
    if ($telemetryToken -cnotmatch '^[A-Za-z0-9._~+/=-]+$') { Fail 'the telemetry token is empty or has invalid characters' }
}
# The token goes into an HTTP header: one line, no surrounding space.
$token = "$token" -replace "[`r`n]", ''
if (-not $token) { Fail 'the enrollment token is empty' }
if ($token -cnotmatch '^[A-Za-z0-9._~+/=-]+$') { Fail "the enrollment token has characters a bearer token can't" }

# Only SYSTEM (the service) and Administrators: set before anything secret is written.
New-Item -ItemType Directory -Force -Path $root | Out-Null
$acl = New-Object System.Security.AccessControl.DirectorySecurity
$acl.SetAccessRuleProtection($true, $false)
foreach ($who in 'S-1-5-18', 'S-1-5-32-544') {
    $sid = New-Object System.Security.Principal.SecurityIdentifier($who)
    $acl.AddAccessRule((New-Object System.Security.AccessControl.FileSystemAccessRule($sid, 'FullControl',
        'ContainerInherit, ObjectInherit', 'None', 'Allow')))
}
Set-Acl -LiteralPath $root -AclObject $acl

function Write-Atomically([string]$path, [string]$content) {
    [System.IO.File]::WriteAllText("$path.tmp", $content, (New-Object System.Text.UTF8Encoding($false)))
    Move-Item -LiteralPath "$path.tmp" -Destination $path -Force
}

# The config's paths, with forward slashes: Go opens them, and confmap's ${file:...} takes them.
function Slashed([string]$path) { $path -replace '\\', '/' }
$programs = Join-Path $env:ProgramFiles 'pydantic-opamp'
$tokenPath = Join-Path $root 'enroll-token'
Write-Atomically $tokenPath $token
# A YAML flow sequence of single-quoted strings (none holds a quote); [] for none.
function YamlList([string[]]$items) { '[' + ((@($items) | Where-Object { $_ } | ForEach-Object { "'$_'" }) -join ', ') + ']' }
# The collector executables the server offers are installed if this repository's release workflow
# signed them (PydanticCosignVerifier in supervisor/config/agent_package.go). A literal here-string:
# the subject regex is written as it is.
$acceptsPackages = if ($AcceptPackages) { "`n  accepts_packages: true" } else { '' }
$package = if ($AcceptPackages) {
    "`n" + @'

  # The collector the OpAMP server offers is installed if this repository's release workflow
  # signed it, and runs while the server offers it; this one runs again when it stops. The
  # server can't change whose signatures are accepted.
  package:
    agent_binary: otelcol.exe
    verifier:
      type: cosign
      cosign:
        github_workflow_repository: pydantic/opamp-clients
        identities:
          - issuer: https://token.actions.githubusercontent.com
            subject_regex: '^https://github\.com/pydantic/opamp-clients/\.github/workflows/release\.yml@refs/(tags/go/opampsupervisor/v[0-9]+\.[0-9]+\.[0-9]+|heads/main)$'
'@
} else { '' }
$telemetryPath = Join-Path $root 'telemetry-token'
if ($TelemetryTokenStdin) { Write-Atomically $telemetryPath $telemetryToken }
$agentEnv = if (Test-Path -LiteralPath $telemetryPath -PathType Leaf) {
    "  env:`n    LOGFIRE_TOKEN: '`${file:$(Slashed $telemetryPath)}'`n"
} else { '' }
# -HostLogs: the event log receiver, and the file_storage extension that keeps where it got to, in
# a directory of the Supervisor's own.
$receivers = '[otlp, host_metrics, hostmetrics, file_log, filelog, nop]'
$extensions = '[health_check]'
$storageDirectories = ''
if ($HostLogs) {
    $receivers = '[otlp, host_metrics, hostmetrics, file_log, filelog, nop, windows_event_log, windowseventlog]'
    $extensions = '[health_check, file_storage]'
    $storageDirectories = "`n    # Where its file_storage extensions may keep files: these directories, and what is under them.`n    allowed_storage_directories: $(YamlList @(Slashed (Join-Path $root 'var\collector')))"
}
# The policy: none unless -Restricted, so every config the server sends runs.
$policy = if ($Restricted) {
@"
  # What the server's remote config may do to this host (-Restricted). The server can't change
  # this file; widen it here, or run pydantic-opamp-enroll.ps1 again with -ExporterEndpoint,
  # -LogPath or -HostLogs.
  remote_config_policy:
    # The component types a Logfire config uses, by their names and their older aliases.
    allowed_components:
      receivers: $receivers
      processors: [batch, memory_limiter, resource, resource_detection, resourcedetection]
      exporters: [otlp_grpc, otlp, otlp_http, otlphttp, debug, nop]
      extensions: $extensions
    # No arbitrary `${...}: only locally bound credential environment names are permitted.
    allowed_providers: []
    # Where its exporters, and the collector's own telemetry, may send.
    allowed_exporter_endpoints: $(YamlList $ExporterEndpoint)
    # What its filelog receivers may read: these files, and what is under these directories.
    allowed_log_paths: $(YamlList @($LogPath | Where-Object { $_ } | ForEach-Object { Slashed $_ }))$storageDirectories
"@
} else {
    "  # Any config the OpAMP server sends runs here. To limit what it may configure, and where it may`n  # send, run pydantic-opamp-enroll.ps1 again with -Restricted."
}
$tls = if ($CaFile) { "  tls:`n    ca_file: '$(Slashed $CaFile)'`n" } else { '' }
Write-Atomically (Join-Path $root 'supervisor.yaml') @"
# Written by pydantic-opamp-enroll. Run it again to enroll with another server or token.
server:
  endpoint: "$Endpoint"
  headers:
    # The enrollment token, readable by SYSTEM and Administrators alone. Once the server has
    # issued this agent its own credentials, they are used instead (and kept across restarts).
    Authorization: 'Bearer `${file:$(Slashed $tokenPath)}'
$tls
capabilities:
  accepts_remote_config: true
  accepts_opamp_connection_settings: true
  accepts_other_connection_settings: true
  accepts_restart_command: true
  reports_effective_config: true
  reports_health: true
  reports_available_components: true$acceptsPackages

agent:
  connection_env:
    logfire/otlp: LOGFIRE_TOKEN
  executable: '$(Slashed (Join-Path $programs 'otelcol.exe'))'
  # A remote config the Collector fails on is replaced by the last one that ran healthy, and stays FAILED.
  automatic_config_rollback: true
  # The Collector's last 4 KiB of log goes with a FAILED config's status, so the server shows why.
  collector_crash_log_snippet_kib: 4
$agentEnv

$policy$package

storage:
  directory: '$(Slashed (Join-Path $root 'var'))'
  # The issued credentials, sealed with a key DPAPI protects for SYSTEM.
  state_key: dpapi
"@

if ($NoStart) {
    Write-Output "enrolled with $Endpoint; start the Supervisor with: Set-Service $service -StartupType Automatic; Start-Service $service"
} else {
    Set-Service -Name $service -StartupType Automatic
    $started = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
    Restart-Service -Name $service
    # An install that can't reach its server (an untrusted certificate, a refused key) fails here,
    # with what to check, instead of reporting success; the service keeps retrying.
    $supervisor = if ($env:OPAMP_SUPERVISOR) { $env:OPAMP_SUPERVISOR } else { Join-Path $programs 'opampsupervisor.exe' }
    $storage = Join-Path $root 'var'
    & $supervisor status --storage $storage --wait 30s --since $started
    if ($LASTEXITCODE -ne 0) {
        Write-Error "enrolled, but the Supervisor can't connect to $Endpoint yet; it keeps retrying. Fix what is above, then check with: & '$supervisor' status --storage '$storage'"
        exit 1
    }
    Write-Output "enrolled with $Endpoint; the Supervisor is running and connected ($service)"
}
