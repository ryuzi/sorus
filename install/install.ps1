# sorus install / update script for Windows (Windows PowerShell 5.1 or later).
#
# Usage (paste into PowerShell and press Enter):
#   irm https://github.com/ryuzi/sorus/releases/latest/download/install.ps1 | iex
#
# What it does (no administrator rights needed):
#   1. Picks the zip for this CPU and downloads it with Invoke-WebRequest (no Mark of the Web).
#   2. Checks its SHA256 against SHA256SUMS of the same release and stops on mismatch
#      (an sorus that is already installed is left untouched).
#   3. Installs %LOCALAPPDATA%\Programs\sorus\sorus.exe and "sorus" shortcuts in the Start Menu
#      and on the Desktop. The shortcuts use the app icon embedded in sorus.exe.
#   4. Starts sorus. Running it again updates sorus in place (user data lives elsewhere).
#
# Environment variables (mainly for tests and advanced users, same as install.sh):
#   SORUS_VERSION       pin a version such as v1.2.3 (default: latest)
#   SORUS_RELEASE_BASE  download base URL (trailing / optional)
#   SORUS_NO_LAUNCH     any non-empty value skips starting sorus
#   SORUS_FORCE_ARCH    used instead of PROCESSOR_ARCHITECTURE (tests)
#
# Keep this file pure ASCII. Japanese messages are written as \uXXXX escapes and decoded
# by J at run time. Windows PowerShell 5.1 decodes "irm | iex" downloads as ISO-8859-1
# (a UTF-8 BOM would even turn into stray characters before the first command) and reads
# BOM-less files in the ANSI code page, so ASCII is the only encoding that survives both.
# install/install_test.go enforces this.

& {
    Set-StrictMode -Version 2.0
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'

    $releases = 'https://github.com/ryuzi/sorus/releases'

    function J([string]$s) { [regex]::Unescape($s) }
    function Say([string]$s) { Write-Host $s }

    $netError = J '\u30c0\u30a6\u30f3\u30ed\u30fc\u30c9\u3067\u304d\u307e\u305b\u3093\u3067\u3057\u305f\u3002\u30a4\u30f3\u30bf\u30fc\u30cd\u30c3\u30c8\u306b\u3064\u306a\u304c\u3063\u3066\u3044\u308b\u304b\u78ba\u304b\u3081\u3066\u3001\u3082\u3046\u4e00\u5ea6\u304a\u8a66\u3057\u304f\u3060\u3055\u3044\u3002'
    $busyError = J 'sorus \u3092\u7f6e\u304d\u63db\u3048\u3089\u308c\u307e\u305b\u3093\u3067\u3057\u305f\u3002sorus \u304c\u52d5\u3044\u3066\u3044\u308b\u5834\u5408\u306f\u3001\u753b\u9762\u306e\u300csorus \u3092\u7d42\u4e86\u300d\u3067\u7d42\u4e86\u3057\u3066\u304b\u3089\u3001\u3082\u3046\u4e00\u5ea6\u3053\u306e1\u884c\u3092\u5b9f\u884c\u3057\u3066\u304f\u3060\u3055\u3044\u3002'

    function Get-File([string]$url, [string]$out) {
        try {
            Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $out
        } catch {
            throw $netError
        }
    }

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ('sorus-install-' + [guid]::NewGuid().ToString('N'))
    try {
        # GitHub requires TLS 1.2, which older .NET defaults do not enable.
        try {
            [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
        } catch {
        }

        $machine = $env:SORUS_FORCE_ARCH
        if (-not $machine) { $machine = $env:PROCESSOR_ARCHITEW6432 }
        if (-not $machine) { $machine = $env:PROCESSOR_ARCHITECTURE }
        switch ($machine) {
            'AMD64' { $arch = 'amd64' }
            'ARM64' { $arch = 'arm64' }
            default { throw ((J '\u3053\u306e CPU\uff08{0}\uff09\u306b\u306f\u5bfe\u5fdc\u3057\u3066\u3044\u307e\u305b\u3093\u3002') -f $machine) }
        }

        if ($env:SORUS_RELEASE_BASE) {
            $base = $env:SORUS_RELEASE_BASE
        } elseif ($env:SORUS_VERSION) {
            $base = "$releases/download/$($env:SORUS_VERSION)/"
        } else {
            $base = "$releases/latest/download/"
        }
        if (-not $base.EndsWith('/')) { $base += '/' }

        New-Item -ItemType Directory -Path $tmp | Out-Null

        Say (J 'sorus \u306e\u30a4\u30f3\u30b9\u30c8\u30fc\u30eb\u3092\u59cb\u3081\u307e\u3059\u3002')
        Say (J '\u30d5\u30a1\u30a4\u30eb\u306e\u4e00\u89a7\u3092\u53d6\u5f97\u3057\u3066\u3044\u307e\u3059...')
        $sums = Join-Path $tmp 'SHA256SUMS'
        Get-File ($base + 'SHA256SUMS') $sums

        # Find the desktop build for this CPU in SHA256SUMS (server builds are skipped).
        $suffix = "-windows-$arch.zip"
        $want = $null
        if ($env:SORUS_VERSION) { $want = "sorus-$($env:SORUS_VERSION)$suffix" }
        $expected = $null
        $archive = $null
        foreach ($line in [IO.File]::ReadAllLines($sums)) {
            $parts = $line.Trim() -split '\s+', 2
            if ($parts.Count -ne 2) { continue }
            $name = $parts[1].TrimStart('*')
            if ($want) {
                if ($name -ne $want) { continue }
            } elseif (-not ($name.StartsWith('sorus-') -and -not $name.StartsWith('sorus-server-') -and $name.EndsWith($suffix) -and $name.Length -gt $suffix.Length + 6)) {
                continue
            }
            $expected = $parts[0].ToLowerInvariant()
            $archive = $name
            break
        }
        if (-not $archive) { throw ((J '\u3053\u306e CPU\uff08{0}\uff09\u5411\u3051\u306e\u30d5\u30a1\u30a4\u30eb\u304c\u30ea\u30ea\u30fc\u30b9\u306b\u3042\u308a\u307e\u305b\u3093\u3002') -f "windows / $arch") }
        $version = $archive.Substring(6, $archive.Length - 6 - $suffix.Length)

        Say ((J 'sorus {0} \u3092\u30c0\u30a6\u30f3\u30ed\u30fc\u30c9\u3057\u3066\u3044\u307e\u3059...') -f $version)
        $zip = Join-Path $tmp $archive
        Get-File ($base + $archive) $zip

        Say (J '\u30d5\u30a1\u30a4\u30eb\u304c\u6b63\u3057\u3044\u304b\u78ba\u304b\u3081\u3066\u3044\u307e\u3059...')
        $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $zip).Hash.ToLowerInvariant()
        if ($actual -ne $expected) {
            throw (J '\u30c0\u30a6\u30f3\u30ed\u30fc\u30c9\u3057\u305f\u30d5\u30a1\u30a4\u30eb\u304c\u58ca\u308c\u3066\u3044\u308b\u304b\u3001\u66f8\u304d\u63db\u3048\u3089\u308c\u3066\u3044\u308b\u53ef\u80fd\u6027\u304c\u3042\u308b\u305f\u3081\u3001\u30a4\u30f3\u30b9\u30c8\u30fc\u30eb\u3092\u4e2d\u6b62\u3057\u307e\u3057\u305f\u3002\u6642\u9593\u3092\u304a\u3044\u3066\u3082\u3046\u4e00\u5ea6\u304a\u8a66\u3057\u304f\u3060\u3055\u3044\u3002\u3059\u3067\u306b\u5165\u3063\u3066\u3044\u308b sorus \u306f\u305d\u306e\u307e\u307e\u4f7f\u3048\u307e\u3059\u3002')
        }

        $x = Join-Path $tmp 'x'
        Expand-Archive -LiteralPath $zip -DestinationPath $x -Force
        $src = Join-Path $x 'sorus.exe'
        if (-not (Test-Path -LiteralPath $src -PathType Leaf)) {
            throw (J '\u30c0\u30a6\u30f3\u30ed\u30fc\u30c9\u3057\u305f\u30d5\u30a1\u30a4\u30eb\u306e\u4e2d\u306b sorus.exe \u304c\u898b\u3064\u304b\u308a\u307e\u305b\u3093\u3002')
        }

        Say (J '\u30a4\u30f3\u30b9\u30c8\u30fc\u30eb\u3057\u3066\u3044\u307e\u3059...')
        $dir = Join-Path $env:LOCALAPPDATA 'Programs\sorus'
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        $exe = Join-Path $dir 'sorus.exe'
        # Copy next to the target and then replace it, so a failure keeps the old sorus.exe.
        $new = Join-Path $dir 'sorus.exe.new'
        Copy-Item -LiteralPath $src -Destination $new -Force
        try {
            Move-Item -LiteralPath $new -Destination $exe -Force
        } catch {
            Remove-Item -LiteralPath $new -Force -ErrorAction SilentlyContinue
            throw $busyError
        }
        Unblock-File -LiteralPath $exe -ErrorAction SilentlyContinue

        try {
            $shell = New-Object -ComObject WScript.Shell
            foreach ($folder in @([Environment]::GetFolderPath('Programs'), [Environment]::GetFolderPath('Desktop'))) {
                if (-not $folder) { continue }
                $lnk = $shell.CreateShortcut((Join-Path $folder 'sorus.lnk'))
                $lnk.TargetPath = $exe
                $lnk.WorkingDirectory = $dir
                $lnk.Description = 'sorus'
                # The app icon is embedded in sorus.exe (scripts/release.sh links build/icons/*.syso).
                # Older releases without it simply show the default program icon.
                $lnk.IconLocation = "$exe,0"
                $lnk.Save()
            }
        } catch {
            Say (J '\u30b7\u30e7\u30fc\u30c8\u30ab\u30c3\u30c8\u3092\u4f5c\u308c\u307e\u305b\u3093\u3067\u3057\u305f\u3002sorus \u306f\u6b21\u306e\u5834\u6240\u304b\u3089\u8d77\u52d5\u3067\u304d\u307e\u3059:')
            Say $exe
        }
        Say ((J '\u30a4\u30f3\u30b9\u30c8\u30fc\u30eb\u3057\u307e\u3057\u305f: {0}') -f $exe)

        if ($env:SORUS_NO_LAUNCH) {
            Say (J '\u5b8c\u4e86\u3057\u307e\u3057\u305f\uff08\u8d77\u52d5\u306f\u7701\u7565\u3057\u307e\u3057\u305f\uff09\u3002')
            return
        }
        Say (J 'sorus \u3092\u8d77\u52d5\u3057\u3066\u3044\u307e\u3059\u3002\u30d6\u30e9\u30a6\u30b6\u3067\u753b\u9762\u304c\u958b\u304d\u307e\u3059...')
        Start-Process -FilePath $exe -WorkingDirectory $dir
        Say (J '\u5b8c\u4e86\u3057\u307e\u3057\u305f\u3002\u6b21\u304b\u3089\u306f\u30b9\u30bf\u30fc\u30c8\u30e1\u30cb\u30e5\u30fc\u304b\u30c7\u30b9\u30af\u30c8\u30c3\u30d7\u306e sorus \u304b\u3089\u8d77\u52d5\u3067\u304d\u307e\u3059\u3002')
    } catch {
        Write-Host ((J '\u30a8\u30e9\u30fc: ') + $_.Exception.Message) -ForegroundColor Red
        # Run as a file (-File): report failure through the exit code. Under "irm | iex",
        # exit would close the user's PowerShell window, so only the message is shown.
        if ($PSCommandPath) { exit 1 }
    } finally {
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}
