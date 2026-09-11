<#
.SYNOPSIS
    Interactive step-by-step wizard to sync extracted game files, generate static data,
    and update Midir.

.DESCRIPTION
    Guides the user through:
      1. Confirming or entering extracted client data folder
      2. Creating a unique versioned snapshot folder in tools/data_versions/
      3. Syncing the 39 required game files
      4. Running the Go data generator to extract icons and build JSONs
      5. Deploying the generated data directly into cmd/dilmeterapi/static_data
      6. Optionally rebuilding Midir.exe
#>

[CmdletBinding()]
param(
    [string]$ConfigFile = "",
    [switch]$NonInteractive
)

$ErrorActionPreference = "Stop"

$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Definition
if (-not $scriptDir) { $scriptDir = $PSScriptRoot }
if (-not $scriptDir) { $scriptDir = (Get-Location).Path }

$toolsDir = Split-Path -Parent $scriptDir
$rootDir = Split-Path -Parent $toolsDir

if (-not $ConfigFile) {
    $ConfigFile = Join-Path $scriptDir "config.json"
}

Clear-Host
Write-Host "========================================================" -ForegroundColor Cyan
Write-Host "          Midir - Game Data Update Wizard               " -ForegroundColor Cyan
Write-Host "========================================================" -ForegroundColor Cyan
Write-Host ""

# 1. Load config
if (-not (Test-Path $ConfigFile)) {
    Write-Error "Configuration file not found at: $ConfigFile"
    exit 1
}

$config = Get-Content -Raw $ConfigFile | ConvertFrom-Json
$currentSource = $config.source_directory
$versionsRel = if ($config.versions_directory) { $config.versions_directory } else { "data_versions" }
$versionsBaseDir = Join-Path $toolsDir $versionsRel
$staticDataDir = Join-Path $rootDir "cmd\dilmeterapi\static_data"

# Step 1: Source Directory Input
Write-Host "[Step 1/5] Extracted Game Files Source Folder" -ForegroundColor Yellow

$chosenSource = $currentSource
if ([string]::IsNullOrWhiteSpace($chosenSource)) {
    Write-Host "Please enter the full path to your extracted game 'data' folder" -ForegroundColor Gray
    Write-Host "(e.g., C:\Games\MabinogiExtracted\data)" -ForegroundColor DarkGray
    Write-Host ""
    if (-not $NonInteractive.IsPresent) {
        while ([string]::IsNullOrWhiteSpace($chosenSource) -or -not (Test-Path $chosenSource)) {
            $inputSrc = Read-Host "Enter source path"
            $chosenSource = $inputSrc.Trim().Trim('"').Trim("'")
            if (-not (Test-Path $chosenSource)) {
                Write-Host "[ERROR] Directory not found: '$chosenSource'. Please check the path and try again." -ForegroundColor Red
            }
        }
    }
} else {
    Write-Host "Previously used source path:" -ForegroundColor Gray
    Write-Host "  $chosenSource" -ForegroundColor White
    Write-Host ""
    if (-not $NonInteractive.IsPresent) {
        $inputSrc = Read-Host "Press [Enter] to reuse this path, or paste a new folder path"
        if ($inputSrc.Trim() -ne "") {
            $chosenSource = $inputSrc.Trim().Trim('"').Trim("'")
        }
        while (-not (Test-Path $chosenSource)) {
            Write-Host ""
            Write-Host "[ERROR] Directory not found: '$chosenSource'" -ForegroundColor Red
            $inputRetry = Read-Host "Please enter a valid extracted 'data' folder path"
            $chosenSource = $inputRetry.Trim().Trim('"').Trim("'")
        }
    }
}

# Save entered source path in config for convenience on future runs
if ($chosenSource -ne $currentSource) {
    $config.source_directory = $chosenSource
    $config | ConvertTo-Json -Depth 5 | Set-Content -Path $ConfigFile -Encoding UTF8
}

Write-Host "Using source: $chosenSource" -ForegroundColor Green
Write-Host ""

# Step 2: Version Name Prompt
Write-Host "[Step 2/5] Version Snapshot Name" -ForegroundColor Yellow
Write-Host "Each sync creates a unique folder to ensure past data is never overwritten." -ForegroundColor Gray
$defaultVersionName = "data_" + (Get-Date -Format "yyyy-MM-dd_HHmmss")
Write-Host "Default version name: $defaultVersionName" -ForegroundColor White

$chosenVersion = $defaultVersionName
if (-not $NonInteractive.IsPresent) {
    $inputVersion = Read-Host "Press [Enter] for default, or type a custom tag (e.g. G26_Patch)"
    if ($inputVersion.Trim() -ne "") {
        $chosenVersion = ($inputVersion.Trim() -replace '[\\/:*?"<>|]', '_')
    }
}

$versionTargetDir = Join-Path $versionsBaseDir $chosenVersion
if (Test-Path $versionTargetDir) {
    $suffix = 1
    while (Test-Path "${versionTargetDir}_$suffix") { $suffix++ }
    $versionTargetDir = "${versionTargetDir}_$suffix"
    $chosenVersion = Split-Path -Leaf $versionTargetDir
}

Write-Host "Target version folder: $versionTargetDir" -ForegroundColor Green
Write-Host ""

# Step 3: Copy Files
Write-Host "[Step 3/5] Copying Data Files..." -ForegroundColor Yellow
New-Item -ItemType Directory -Path $versionTargetDir -Force | Out-Null

$copiedCount = 0
$missingCount = 0
$fileList = $config.files

foreach ($relPath in $fileList) {
    $srcFile = Join-Path $chosenSource $relPath
    $dstFile = Join-Path $versionTargetDir $relPath

    if (-not (Test-Path $srcFile)) {
        Write-Host "  [MISSING] $relPath" -ForegroundColor Red
        $missingCount++
        continue
    }

    $parentDir = Split-Path -Parent $dstFile
    if (-not (Test-Path $parentDir)) {
        New-Item -ItemType Directory -Path $parentDir -Force | Out-Null
    }

    Copy-Item -Path $srcFile -Destination $dstFile -Force
    $copiedCount++
}

# Update default tools/data mirror as well
$defaultToolsData = Join-Path $toolsDir "data"
if (-not (Test-Path $defaultToolsData)) {
    New-Item -ItemType Directory -Path $defaultToolsData -Force | Out-Null
}
Copy-Item -Path "$versionTargetDir\*" -Destination $defaultToolsData -Recurse -Force

Write-Host "  Successfully copied $copiedCount files to snapshot '$chosenVersion'!" -ForegroundColor Green
if ($missingCount -gt 0) {
    Write-Host "  Warning: $missingCount files were missing in the source directory." -ForegroundColor DarkYellow
}
Write-Host ""

# Step 4: Run Data Generator
Write-Host "[Step 4/5] Generate JSONs and Extracted Icons" -ForegroundColor Yellow
$runGen = "Y"
if (-not $NonInteractive.IsPresent) {
    $runGen = Read-Host "Do you want to run the Data Generator now? [Y/n]"
}

if ($runGen.Trim() -eq "" -or $runGen.Trim().ToUpper() -eq "Y") {
    Write-Host "Running Data Generator..." -ForegroundColor Cyan
    $generatorDir = Join-Path $toolsDir "generator"
    
    $genProc = Start-Process -FilePath "go" -ArgumentList "run", ".", "-data", "$versionTargetDir" -WorkingDirectory $generatorDir -NoNewWindow -PassThru -Wait
    
    if ($genProc.ExitCode -ne 0) {
        Write-Host "[ERROR] Data generator encountered an error." -ForegroundColor Red
    } else {
        Write-Host "Generator finished successfully!" -ForegroundColor Green
        
        # Deploy generated static_data to cmd/dilmeterapi/static_data
        $genOutDir = Join-Path $generatorDir "out\static_data"
        if (Test-Path $genOutDir) {
            Write-Host "Deploying generated data to '$staticDataDir'..." -ForegroundColor Cyan
            
            # Copy JSON files
            Get-ChildItem -Path $genOutDir -Filter "*.json" | ForEach-Object {
                Copy-Item -Path $_.FullName -Destination $staticDataDir -Force
                Write-Host "  Updated: $($_.Name)" -ForegroundColor Gray
            }
            
            # Copy images
            $genImgDir = Join-Path $genOutDir "images"
            if (Test-Path $genImgDir) {
                $targetImgDir = Join-Path $staticDataDir "images"
                if (-not (Test-Path $targetImgDir)) {
                    New-Item -ItemType Directory -Path $targetImgDir -Force | Out-Null
                }
                Copy-Item -Path "$genImgDir\*" -Destination $targetImgDir -Recurse -Force
                Write-Host "  Updated icon images." -ForegroundColor Gray
            }
            
            Write-Host "Backend static data is up to date!" -ForegroundColor Green
        }
    }
} else {
    Write-Host "Skipped data generation." -ForegroundColor Gray
}
Write-Host ""

# Step 5: Rebuild Midir
Write-Host "[Step 5/5] Rebuild Midir Executable" -ForegroundColor Yellow
$runBuild = "Y"
if (-not $NonInteractive.IsPresent) {
    $runBuild = Read-Host "Do you want to build Midir.exe now? [Y/n]"
}

if ($runBuild.Trim() -eq "" -or $runBuild.Trim().ToUpper() -eq "Y") {
    Write-Host "Building Midir.exe..." -ForegroundColor Cyan
    $buildOutDir = Join-Path $rootDir "build"
    if (-not (Test-Path $buildOutDir)) {
        New-Item -ItemType Directory -Path $buildOutDir -Force | Out-Null
    }
    
    $buildArgs = @("build", "-ldflags=`"-s -w`"", "-trimpath", "-o", "build/Midir.exe", "./cmd/dilmeterapi")
    $buildProc = Start-Process -FilePath "go" -ArgumentList $buildArgs -WorkingDirectory $rootDir -NoNewWindow -PassThru -Wait
    
    if ($buildProc.ExitCode -eq 0) {
        Write-Host "========================================================" -ForegroundColor Green
        Write-Host " SUCCESS! Midir.exe has been updated in the build folder. " -ForegroundColor Green
        Write-Host "========================================================" -ForegroundColor Green
    } else {
        Write-Host "[ERROR] Build failed. Please run build.bat for detailed output." -ForegroundColor Red
    }
} else {
    Write-Host "Skipped build. You can run 'build.bat' whenever you are ready." -ForegroundColor Gray
}

Write-Host ""
Write-Host "Done!" -ForegroundColor Cyan
