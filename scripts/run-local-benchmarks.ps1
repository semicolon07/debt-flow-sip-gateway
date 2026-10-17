[CmdletBinding()]
param(
    [string]$OutputDirectory = (Join-Path $PWD "benchmark-results"),
    [int]$Count = 5,
    [string]$Benchtime = "2s"
)

$ErrorActionPreference = "Stop"
$repository = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory)
New-Item -ItemType Directory -Path $OutputDirectory -Force | Out-Null
$packages = @("capability", "protocol", "media", "gateway", "store")

Push-Location $repository
try {
    foreach ($package in $packages) {
        $output = Join-Path $OutputDirectory "$package.txt"
        go test -run '^$' -bench 'Benchmark' -benchmem -count $Count -benchtime $Benchtime "./internal/$package" |
            Tee-Object -FilePath $output
        if ($LASTEXITCODE -ne 0) { throw "benchmark failed: $package" }
    }
    go version | Set-Content -Encoding utf8 (Join-Path $OutputDirectory "go-version.txt")
    Get-ComputerInfo -Property WindowsProductName, WindowsVersion, OsBuildNumber, CsProcessors,
        CsNumberOfLogicalProcessors, CsTotalPhysicalMemory |
        ConvertTo-Json -Depth 4 | Set-Content -Encoding utf8 (Join-Path $OutputDirectory "host.json")
} finally {
    Pop-Location
}

Write-Host "Benchmark results: $OutputDirectory"
