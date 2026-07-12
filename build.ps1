<#
.SYNOPSIS
    Builds the hot-clone linux/amd64 binary into the project root.
#>
$ErrorActionPreference = "Stop"

Push-Location $PSScriptRoot
try {
    $out = Join-Path $PSScriptRoot "hot-clone"

    Write-Host "Building hot-clone for linux/amd64 -> $out"
    $env:GOOS = "linux"
    $env:GOARCH = "amd64"
    go build -o $out .
    if ($LASTEXITCODE -ne 0) {
        throw "go build failed with exit code $LASTEXITCODE"
    }

    Write-Host "Build succeeded: $out"
}
finally {
    Remove-Item Env:\GOOS -ErrorAction SilentlyContinue
    Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue
    Pop-Location
}
