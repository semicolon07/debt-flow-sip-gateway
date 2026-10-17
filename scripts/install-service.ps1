[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [Parameter(Mandatory = $true)]
    [string]$BinaryPath,
    [string]$InstallDirectory = "$env:ProgramFiles\DebtFlow\SipGateway",
    [string]$DataDirectory = "$env:ProgramData\DebtFlow\SipGateway",
    [string]$ServiceName = "DebtFlowSipGateway"
)

$ErrorActionPreference = "Stop"
# Run from an elevated PowerShell prompt. The virtual service account is created by SCM.
$source = (Resolve-Path $BinaryPath).Path
$target = Join-Path $InstallDirectory "sip-gateway.exe"
$config = Join-Path $DataDirectory "config.yaml"
$dataPath = Join-Path $DataDirectory "data"
$logsPath = Join-Path $DataDirectory "logs"
$keysPath = Join-Path $DataDirectory "keys"

function Invoke-ServiceControl {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)
    & sc.exe @Arguments | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw "sc.exe $($Arguments[0]) failed with exit code $LASTEXITCODE"
    }
}

function Set-DirectoryAcl {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$ServiceRights
    )
    & icacls.exe $Path /inheritance:r /grant:r `
        "Administrators:(OI)(CI)F" `
        "SYSTEM:(OI)(CI)F" `
        "NT SERVICE\${ServiceName}:(OI)(CI)$ServiceRights" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "failed to apply ACL to $Path" }
}

if (-not $PSCmdlet.ShouldProcess($target, "Install Debt Flow SIP Gateway Windows Service")) { return }

$existingService = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($null -ne $existingService -and $existingService.Status -ne [System.ServiceProcess.ServiceControllerStatus]::Stopped) {
    Stop-Service -Name $ServiceName -Force -ErrorAction Stop
    $existingService.WaitForStatus([System.ServiceProcess.ServiceControllerStatus]::Stopped, [TimeSpan]::FromSeconds(30))
}

New-Item -ItemType Directory -Path $InstallDirectory -Force | Out-Null
New-Item -ItemType Directory -Path $dataPath -Force | Out-Null
New-Item -ItemType Directory -Path $logsPath -Force | Out-Null
Copy-Item $source $target -Force

if (-not (Test-Path $config)) {
    $sample = Join-Path (Split-Path $source) "config.example.yaml"
    if (-not (Test-Path $sample)) { throw "config.example.yaml was not found beside the binary" }
    Copy-Item $sample $config
}

& $target --config $config --check-config
if ($LASTEXITCODE -ne 0) { throw "configuration validation failed; service was not created" }

if ($null -ne $existingService) {
    Invoke-ServiceControl -Arguments @(
        "config", $ServiceName,
        "binPath=", "`"$target`" --config `"$config`"",
        "start=", "delayed-auto",
        "obj=", "NT SERVICE\$ServiceName"
    )
} else {
    Invoke-ServiceControl -Arguments @(
        "create", $ServiceName,
        "binPath=", "`"$target`" --config `"$config`"",
        "start=", "delayed-auto",
        "obj=", "NT SERVICE\$ServiceName",
        "DisplayName=", "Debt Flow SIP Gateway"
    )
}

Invoke-ServiceControl -Arguments @("sidtype", $ServiceName, "unrestricted")

# The service must exist before Windows can resolve NT SERVICE\<name>. Keep the
# key ring read-only while allowing SQLite/WAL and rotating logs to be written.
Set-DirectoryAcl -Path $DataDirectory -ServiceRights "RX"
Set-DirectoryAcl -Path $dataPath -ServiceRights "M"
Set-DirectoryAcl -Path $logsPath -ServiceRights "M"
if (Test-Path -LiteralPath $keysPath -PathType Container) {
    Set-DirectoryAcl -Path $keysPath -ServiceRights "RX"
}

Invoke-ServiceControl -Arguments @("description", $ServiceName, "Debt Flow browser WebRTC to Issabel/Asterisk SIP gateway")
Invoke-ServiceControl -Arguments @("failure", $ServiceName, "reset=", "86400", "actions=", "restart/5000/restart/15000/restart/60000")
Invoke-ServiceControl -Arguments @("failureflag", $ServiceName, "1")
Start-Service -Name $ServiceName -ErrorAction Stop
$installedService = Get-Service -Name $ServiceName -ErrorAction Stop
$installedService.WaitForStatus([System.ServiceProcess.ServiceControllerStatus]::Running, [TimeSpan]::FromSeconds(30))

