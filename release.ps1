<#
.SYNOPSIS
    Builds hot-clone, pushes any pending changes, and publishes a GitHub
    release using the top dated version entry in CHANGELOG.md.

.DESCRIPTION
    Expects CHANGELOG.md to follow Keep a Changelog style, e.g.:

        ## [1.2.3] - 2026-07-12

        ### Fixed
        - ...

    The first such heading found is used as the release version and its
    body (up to the next "## " heading) becomes the release notes. Move
    entries out of "## Unreleased" into a new dated heading before running
    this script.

    Refuses to re-publish a version that is already tagged or released.
#>
$ErrorActionPreference = "Stop"

Push-Location $PSScriptRoot
try {
    # 1. Build
    & (Join-Path $PSScriptRoot "build.ps1")
    $binary = Join-Path $PSScriptRoot "hot-clone-linux-amd64"
    if (-not (Test-Path $binary)) {
        throw "Expected build output not found: $binary"
    }

    # 2. Parse the top version entry out of CHANGELOG.md
    $changelogPath = Join-Path $PSScriptRoot "CHANGELOG.md"
    if (-not (Test-Path $changelogPath)) {
        throw "CHANGELOG.md not found"
    }
    $lines = Get-Content $changelogPath

    $versionHeaderPattern = '^##\s*\[(?<version>[0-9][^\]]*)\]\s*-\s*(?<date>.+?)\s*$'
    $startIdx = -1
    $version = $null
    for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -match $versionHeaderPattern) {
            $startIdx = $i
            $version = $Matches['version']
            break
        }
    }
    if ($startIdx -eq -1) {
        throw "No '## [x.y.z] - date' entry found in CHANGELOG.md. Add a dated version heading (move it out of Unreleased) before releasing."
    }

    $endIdx = $lines.Count
    for ($i = $startIdx + 1; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -match '^##\s') {
            $endIdx = $i
            break
        }
    }

    $notes = ($lines[($startIdx + 1)..($endIdx - 1)] -join "`n").Trim()
    if ([string]::IsNullOrWhiteSpace($notes)) {
        throw "Changelog entry for $version has no content."
    }

    $tag = "v$version"
    Write-Host "Preparing release $tag"

    # 3. Refuse to redo an existing release/tag
    $existingTag = git tag --list $tag
    if ($existingTag) {
        throw "Tag $tag already exists locally. Bump the version in CHANGELOG.md before releasing again."
    }
    gh release view $tag *> $null
    if ($LASTEXITCODE -eq 0) {
        throw "GitHub release $tag already exists."
    }

    # 4. Commit any pending changes
    git add -A
    $staged = git status --porcelain
    if ($staged) {
        git commit -m "Release $tag"
        if ($LASTEXITCODE -ne 0) { throw "git commit failed" }
    }
    else {
        Write-Host "No pending changes to commit."
    }

    # 5. Push the branch
    $branch = git rev-parse --abbrev-ref HEAD
    git push -u origin $branch
    if ($LASTEXITCODE -ne 0) { throw "git push failed" }

    # 6. Tag and push the tag
    git tag -a $tag -m "Release $tag"
    if ($LASTEXITCODE -ne 0) { throw "git tag failed" }
    git push origin $tag
    if ($LASTEXITCODE -ne 0) { throw "git push of tag failed" }

    # 7. Create the GitHub release with the built binary attached
    $notesFile = Join-Path $env:TEMP "hot-clone-release-notes-$version.md"
    Set-Content -Path $notesFile -Value $notes -NoNewline

    gh release create $tag $binary --title $tag --notes-file $notesFile
    if ($LASTEXITCODE -ne 0) { throw "gh release create failed" }

    Remove-Item $notesFile -ErrorAction SilentlyContinue
    Write-Host "Released $tag successfully."
}
finally {
    Pop-Location
}
