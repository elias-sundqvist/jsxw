param(
    [string]$OutputPath = (Join-Path (Resolve-Path (Join-Path $PSScriptRoot "..")).Path "jsx-window.exe")
)

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path

if (-not (Test-Path (Join-Path $repoRoot "node_modules"))) {
    throw "Missing node_modules in $repoRoot. Run npm install first."
}

Push-Location $repoRoot
try {
    go build -ldflags='-H=windowsgui' -o $OutputPath .
    if ($LASTEXITCODE -ne 0) {
        throw "go build failed."
    }
}
finally {
    Pop-Location
}

Write-Host "Built $OutputPath"
