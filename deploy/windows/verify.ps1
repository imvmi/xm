$ErrorActionPreference = 'Stop'
try {
    # Windows PowerShell 5.1 emits a JSON array as one pipeline object.
    # Assign it directly: wrapping that pipeline in @() creates a nested array.
    $json = Get-Content -LiteralPath (Join-Path -Path $PSScriptRoot -ChildPath 'manifest.json') -Raw -Encoding UTF8
    $entries = ConvertFrom-Json -InputObject $json
    $total = @($entries).Count
    if ($total -eq 0) {
        throw 'The manifest is empty.'
    }
    $failures = 0
    $checked = 0
    foreach ($entry in $entries) {
        if ($null -eq $entry -or $entry.path -isnot [string] -or [string]::IsNullOrWhiteSpace($entry.path)) {
            throw 'Invalid manifest entry: path must be a non-empty string.'
        }
        $path = Join-Path -Path $PSScriptRoot -ChildPath $entry.path
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
            Write-Host "MISSING: $($entry.path)" -ForegroundColor Red
            $failures++
        } else {
            $file = Get-Item -LiteralPath $path
            if ($file.Length -ne $entry.bytes) {
                Write-Host "SIZE MISMATCH: $($entry.path)" -ForegroundColor Red
                $failures++
            } elseif ((Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash -ne $entry.sha256) {
                Write-Host "HASH MISMATCH: $($entry.path)" -ForegroundColor Red
                $failures++
            }
        }
        $checked++
        if ($checked % 100 -eq 0) {
            Write-Progress -Activity 'Verifying XM migration (run before first startup)' -Status "$checked / $total" -PercentComplete (100 * $checked / $total)
        }
    }
    Write-Progress -Activity 'Verifying XM migration (run before first startup)' -Completed
    if ($failures -gt 0) {
        Write-Host "$failures files failed verification. Do not remove the source data." -ForegroundColor Red
        exit 1
    }
    Write-Host "OK: all $checked files passed SHA256 verification. You can now run start.bat." -ForegroundColor Green
} catch {
    Write-Host $_ -ForegroundColor Red
    Write-Host $_.InvocationInfo.PositionMessage -ForegroundColor Red
    exit 1
}
