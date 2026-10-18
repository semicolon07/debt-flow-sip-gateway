[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [Parameter(Mandatory = $true)]
    [string]$BinaryPath,
    [string]$InstallDirectory = "$env:ProgramFiles\DebtFlow\SipGateway",
    [string]$DataDirectory = "$env:ProgramData\DebtFlow\SipGateway",
    [string]$ServiceName = "DebtFlowSipGateway",
    [string]$ExpectedSignerThumbprint,
    [switch]$RequireSignature
)

$ErrorActionPreference = "Stop"
# Run from an elevated PowerShell prompt. The virtual service account is created by SCM.
$source = (Resolve-Path $BinaryPath).Path
$target = Join-Path $InstallDirectory "sip-gateway.exe"
$config = Join-Path $DataDirectory "config.yaml"
$dataPath = Join-Path $DataDirectory "data"
$logsPath = Join-Path $DataDirectory "logs"
$keysPath = Join-Path $DataDirectory "keys"
$stagedTarget = Join-Path $InstallDirectory "sip-gateway.exe.installing-$([guid]::NewGuid().ToString('N'))"
$rollbackTarget = Join-Path $InstallDirectory "sip-gateway.exe.rollback"

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

if ($RequireSignature) {
    if ([string]::IsNullOrWhiteSpace($ExpectedSignerThumbprint)) {
        throw "-ExpectedSignerThumbprint is required with -RequireSignature"
    }
    $signature = Get-AuthenticodeSignature -LiteralPath $source
    $expectedThumbprint = $ExpectedSignerThumbprint.Replace(" ", "").ToUpperInvariant()
    $actualThumbprint = if ($null -ne $signature.SignerCertificate) {
        $signature.SignerCertificate.Thumbprint.Replace(" ", "").ToUpperInvariant()
    } else {
        ""
    }
    if ($signature.Status -ne "Valid" -or $actualThumbprint -ne $expectedThumbprint) {
        throw "source executable must have a valid Authenticode signature from the expected signer"
    }
}

New-Item -ItemType Directory -Path $InstallDirectory -Force | Out-Null
New-Item -ItemType Directory -Path $dataPath -Force | Out-Null
New-Item -ItemType Directory -Path $logsPath -Force | Out-Null

if (-not (Test-Path $config)) {
    $sample = Join-Path (Split-Path $source) "config.example.yaml"
    if (-not (Test-Path $sample)) { throw "config.example.yaml was not found beside the binary" }
    Copy-Item $sample $config
}

Copy-Item $source $stagedTarget -Force
try {
    & $stagedTarget --config $config --check-config
    if ($LASTEXITCODE -ne 0) { throw "configuration validation failed; installed binary was not replaced" }
} catch {
    Remove-Item -LiteralPath $stagedTarget -Force -ErrorAction SilentlyContinue
    throw
}

$existingService = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
$serviceCreated = $false
$binaryReplaced = $false
$rollbackAvailable = Test-Path -LiteralPath $target -PathType Leaf
try {
    if ($null -ne $existingService -and $existingService.Status -ne [System.ServiceProcess.ServiceControllerStatus]::Stopped) {
        Stop-Service -Name $ServiceName -Force -ErrorAction Stop
        $existingService.WaitForStatus([System.ServiceProcess.ServiceControllerStatus]::Stopped, [TimeSpan]::FromSeconds(30))
    }
    if ($rollbackAvailable) {
        Copy-Item -LiteralPath $target -Destination $rollbackTarget -Force
    }
    Move-Item -LiteralPath $stagedTarget -Destination $target -Force
    $binaryReplaced = $true

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
        $serviceCreated = $true
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
} catch {
    if ($serviceCreated) {
        & sc.exe stop $ServiceName 2>$null | Out-Null
        & sc.exe delete $ServiceName 2>$null | Out-Null
    } elseif ($null -ne $existingService) {
        Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
    }
    if ($binaryReplaced -and $rollbackAvailable -and (Test-Path -LiteralPath $rollbackTarget -PathType Leaf)) {
        Copy-Item -LiteralPath $rollbackTarget -Destination $target -Force
    } elseif ($binaryReplaced -and -not $rollbackAvailable) {
        Remove-Item -LiteralPath $target -Force -ErrorAction SilentlyContinue
    }
    if ($null -ne $existingService -and $rollbackAvailable) {
        Start-Service -Name $ServiceName -ErrorAction SilentlyContinue
    }
    throw
} finally {
    Remove-Item -LiteralPath $stagedTarget -Force -ErrorAction SilentlyContinue
}

