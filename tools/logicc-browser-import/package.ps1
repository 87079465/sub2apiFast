$ErrorActionPreference = 'Stop'
$repositoryRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
$downloadDirectory = Join-Path $repositoryRoot 'frontend/public/downloads'
New-Item -ItemType Directory -Path $downloadDirectory -Force | Out-Null
$extensionFiles = @('manifest.json', 'popup.html', 'popup.css', 'popup.mjs', 'bridge.mjs', 'README.md') |
    ForEach-Object { Join-Path $PSScriptRoot $_ }
Compress-Archive -LiteralPath $extensionFiles -DestinationPath (Join-Path $downloadDirectory 'logicc-browser-import.zip') -Force
Write-Output 'Packaged frontend/public/downloads/logicc-browser-import.zip'
