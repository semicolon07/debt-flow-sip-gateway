[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[A-Za-z0-9._-]{1,128}$')]
    [string]$KeyId,
    [SecureString]$Base64Key,
    [string]$DataDirectory = "$env:ProgramData\DebtFlow\SipGateway",
    [string]$ServiceName = "DebtFlowSipGateway"
)

$ErrorActionPreference = "Stop"
if ([System.Environment]::OSVersion.Platform -ne [System.PlatformID]::Win32NT) {
    throw "Capability key provisioning must run on Windows."
}
if ($null -eq $Base64Key) {
    $Base64Key = Read-Host "Base64-encoded 32-byte capability key" -AsSecureString
}

$plainText = [System.Net.NetworkCredential]::new("", $Base64Key).Password
$rawKey = $null
$protected = $null
try {
    $rawKey = [Convert]::FromBase64String($plainText)
    if ($rawKey.Length -ne 32) { throw "Capability key must contain exactly 32 bytes." }
    $protected = [System.Security.Cryptography.ProtectedData]::Protect(
        $rawKey,
        $null,
        [System.Security.Cryptography.DataProtectionScope]::LocalMachine)
    $keysDirectory = Join-Path $DataDirectory "keys"
    $target = Join-Path $keysDirectory ($KeyId + ".dpapi")
    if (-not $PSCmdlet.ShouldProcess($target, "Store DPAPI LocalMachine protected capability key")) { return }
    New-Item -ItemType Directory -Path $keysDirectory -Force | Out-Null
    & icacls.exe $keysDirectory /inheritance:r | Out-Null
    & icacls.exe $keysDirectory /grant:r "Administrators:(OI)(CI)F" "SYSTEM:(OI)(CI)F" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Failed to apply capability key ACL." }
    $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if ($null -ne $service) {
        & icacls.exe $keysDirectory /grant:r "NT SERVICE\${ServiceName}:(OI)(CI)RX" | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "Failed to grant the service read access to the capability key." }
    } else {
        Write-Verbose "Service is not registered yet; install-service.ps1 will grant its read-only key ACL."
    }
    [System.IO.File]::WriteAllText($target, [Convert]::ToBase64String($protected))
    Write-Output $target
}
finally {
    $plainText = $null
    if ($null -ne $rawKey) { [Array]::Clear($rawKey, 0, $rawKey.Length) }
    if ($null -ne $protected) { [Array]::Clear($protected, 0, $protected.Length) }
}
