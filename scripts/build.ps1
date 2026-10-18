[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$')]
    [string]$Version,
    [string]$OutputDirectory = (Join-Path $PSScriptRoot "..\dist"),
    [string]$CodeSigningCertificateThumbprint,
    [string]$TimestampUrl = "http://timestamp.digicert.com",
    [switch]$RequireSignature
)

$ErrorActionPreference = "Stop"
$repository = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory)
New-Item -ItemType Directory -Path $OutputDirectory -Force | Out-Null

$gitCommitOutput = & git -C $repository rev-parse HEAD 2>$null
$hasGitCommit = $LASTEXITCODE -eq 0 -and
    ([string]$gitCommitOutput).Trim() -match '^[0-9a-fA-F]{40,64}$'
$gitCommit = if ($hasGitCommit) { ([string]$gitCommitOutput).Trim().ToLowerInvariant() } else { "uncommitted" }
$gitStatusOutput = & git -C $repository status --porcelain
if ($LASTEXITCODE -ne 0) { throw "git status failed" }
$sourceModified = (-not $hasGitCommit) -or [bool]$gitStatusOutput
if ($RequireSignature -and (-not $hasGitCommit -or $sourceModified)) {
    throw "signed release builds require a committed, clean Git worktree"
}
$existingOutput = @(Get-ChildItem -LiteralPath $OutputDirectory -Force)
if ($RequireSignature -and $existingOutput.Count -ne 0) {
    throw "signed release builds require a new or empty output directory"
}

$env:CGO_ENABLED = "0"
$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -trimpath -buildvcs=true `
    -ldflags "-s -w -X main.version=$Version" `
    -o (Join-Path $OutputDirectory "sip-gateway.exe") `
    (Join-Path $repository "cmd/sip-gateway")
if ($LASTEXITCODE -ne 0) { throw "go build failed" }

Copy-Item (Join-Path $repository "config.example.yaml") (Join-Path $OutputDirectory "config.example.yaml") -Force
foreach ($directory in @("deploy", "docs", "scripts", "test")) {
    Copy-Item (Join-Path $repository $directory) (Join-Path $OutputDirectory $directory) -Recurse -Force
}
go version -m (Join-Path $OutputDirectory "sip-gateway.exe") |
    Set-Content -Encoding utf8 (Join-Path $OutputDirectory "build-info.txt")

$moduleRecords = @(go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}}{{"\t"}}{{if .Version}}{{.Version}}{{else}}unknown{{end}}{{end}}{{end}}' ./cmd/sip-gateway)
if ($LASTEXITCODE -ne 0) { throw "go list failed while generating the SBOM" }
$moduleComponents = @($moduleRecords | Sort-Object -Unique |
    Where-Object { -not [string]::IsNullOrWhiteSpace($_) } |
    ForEach-Object {
        $fields = $_ -split "`t", 2
        if ($fields.Count -ne 2 -or [string]::IsNullOrWhiteSpace($fields[0]) -or
            [string]::IsNullOrWhiteSpace($fields[1])) {
            throw "go list returned an invalid module record"
        }
        [ordered]@{
            type = "library"
            name = $fields[0]
            version = $fields[1]
            purl = "pkg:golang/$($fields[0])@$($fields[1])"
        }
})
[ordered]@{
    bomFormat = "CycloneDX"
    specVersion = "1.6"
    serialNumber = "urn:uuid:$([guid]::NewGuid())"
    version = 1
    metadata = [ordered]@{
        timestamp = [DateTime]::UtcNow.ToString("o")
        component = [ordered]@{ type = "application"; name = "debt-flow-sip-gateway"; version = $Version }
    }
    components = $moduleComponents
} | ConvertTo-Json -Depth 8 | Set-Content -Encoding utf8 (Join-Path $OutputDirectory "sbom.cdx.json")

[ordered]@{
    schemaVersion = 1
    version = $Version
    gitCommit = $gitCommit
    sourceModified = $sourceModified
    targetOs = "windows"
    targetArchitecture = "amd64"
    generatedAtUtc = [DateTime]::UtcNow.ToString("o")
} | ConvertTo-Json | Set-Content -Encoding utf8 (Join-Path $OutputDirectory "release-manifest.json")

$binary = Join-Path $OutputDirectory "sip-gateway.exe"
if ($CodeSigningCertificateThumbprint) {
    $signTool = Get-Command signtool.exe -ErrorAction Stop
    & $signTool.Source sign /sha1 $CodeSigningCertificateThumbprint /fd SHA256 /tr $TimestampUrl /td SHA256 $binary
    if ($LASTEXITCODE -ne 0) { throw "Authenticode signing failed" }
    & $signTool.Source verify /pa $binary
    if ($LASTEXITCODE -ne 0) { throw "Authenticode signature verification failed" }
} elseif ($RequireSignature) {
    throw "-RequireSignature requires -CodeSigningCertificateThumbprint"
}

Get-ChildItem -Path $OutputDirectory -Recurse -File |
    Where-Object { $_.Name -ne "SHA256SUMS" } |
    Sort-Object FullName |
    ForEach-Object {
        $relativePath = $_.FullName.Substring($OutputDirectory.TrimEnd('\').Length + 1).Replace('\', '/')
        $hash = (Get-FileHash -Algorithm SHA256 $_.FullName).Hash.ToLowerInvariant()
        "$hash  $relativePath"
    } |
    Set-Content -Encoding ascii (Join-Path $OutputDirectory "SHA256SUMS")

