# Checks of neodesk.ps1's service set-up for the server with its web
# console, without Windows: the installer's functions are loaded from the
# script's syntax tree and the Windows-only commands (the service manager,
# scheduled tasks, icacls) are replaced by stand-ins.
#
#   pwsh -NoProfile -File scripts/test-windows-installer.ps1
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
$installer = Join-Path $root 'install/neodesk.ps1'
$tokens = $null
$parseErrors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile($installer, [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count -gt 0) { throw "neodesk.ps1 does not parse: $($parseErrors[0].Message)" }
foreach ($fn in $ast.FindAll({ $args[0] -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    . ([scriptblock]::Create($fn.Extent.Text))
}

$script:failures = 0
function Fail([string]$Message) { Write-Host "FAIL: $Message"; $script:failures++ }

# Stand-ins for Windows: the service manager holds one service,
# NeoDeskServer, or none; an older install's NSSM settings are a table
$script:registeredTasks = @{}
$script:fakeService = $null
$script:scCalls = @()
$script:nssmSettings = @{}
function Get-ServerServiceInfo { $script:fakeService }
function New-Service {
    param($Name, $BinaryPathName, $DisplayName, $Description, $StartupType, $ErrorAction)
    $script:fakeService = [pscustomobject]@{ PathName = $BinaryPathName; StartName = 'LocalSystem'; DisplayName = $DisplayName; StartupType = "$StartupType"; Environment = @() }
}
function Set-Service {
    param($Name, $DisplayName, $Description, $StartupType, $ErrorAction)
    if ($script:fakeService) { $script:fakeService.DisplayName = $DisplayName; $script:fakeService.StartupType = "$StartupType" }
}
function Set-ServerServicePath { param($PathName) if (-not $script:fakeService) { return $false }; $script:fakeService.PathName = $PathName; return $true }
function Get-ServerServiceEnvironment { if ($script:fakeService) { @($script:fakeService.Environment) } else { @() } }
function Set-ServerServiceEnvironment { param([string[]]$Environment) $script:fakeService.Environment = @($Environment) }
function Invoke-ServiceControl {
    $call = @($args | ForEach-Object { "$_" })
    $script:scCalls += , $call
    if ($call[0] -eq 'config' -and $call[2] -eq 'obj=' -and $script:fakeService) { $script:fakeService.StartName = $call[3] }
    if ($call[0] -eq 'delete' -and $call[1] -eq 'NeoDeskServer') { $script:fakeService = $null }
    return 0
}
function Set-ServerServiceAccount { param($Account) $script:fakeService.StartName = $Account; return $true }
function Get-NssmSetting { param($Name) $script:nssmSettings[$Name] }
function Remove-NssmSettings { $script:nssmSettings = @{} }
function Stop-Service { param($Name, [switch]$Force, $ErrorAction) }
function Start-Service { param($Name, $ErrorAction) }
function Stop-ScheduledTask { param($TaskName, $ErrorAction) }
# No test ends a process of the machine it runs on
function Stop-Process { param([Parameter(ValueFromPipeline = $true)]$InputObject, $Name, $Id, [switch]$Force) process { } }
function Test-BinaryRunsAsService { param($Path) $true }
$script:leastPrivilege = @()
function Set-ServiceLeastPrivilege { param($ServiceName, $Paths) $script:leastPrivilege += , @($ServiceName, $Paths) }
$script:icaclsCalls = @()
function icacls { $script:icaclsCalls += , @($args | ForEach-Object { "$_" }); $global:LASTEXITCODE = 0 }
function Unregister-ScheduledTask { param($TaskName, [switch]$Confirm, $ErrorAction) $script:registeredTasks.Remove($TaskName) }
function New-ScheduledTaskAction { param($Execute, $Argument, $WorkingDirectory) [pscustomobject]@{ Execute = $Execute; Arguments = $Argument } }
function New-ScheduledTaskTrigger { param([switch]$AtStartup) 'startup' }
function New-ScheduledTaskPrincipal { param($UserId, $LogonType, $RunLevel) $UserId }
function New-ScheduledTaskSettingsSet { param([switch]$AllowStartIfOnBatteries, [switch]$DontStopIfGoingOnBatteries, $RestartCount, $RestartInterval, $ExecutionTimeLimit) [pscustomobject]@{ ExecutionTimeLimit = $ExecutionTimeLimit } }
function Register-ScheduledTask { param($TaskName, $Action, $Trigger, $Principal, $Settings, $Description, $Xml) $script:registeredTasks[$TaskName] = $Settings }
function Get-ScheduledTask { param($TaskName, $ErrorAction) if ($script:registeredTasks.ContainsKey($TaskName)) { [pscustomobject]@{ TaskName = $TaskName } } }
function Get-Service { param($Name, $ErrorAction) if ($script:fakeService -and $Name -eq 'NeoDeskServer') { [pscustomobject]@{ Name = $Name; Status = 'Stopped' } } }
# Windows Firewall: the rules in a list
$script:fwRules = @()
function Get-NetFirewallRule {
    param($DisplayName, $Direction, $Enabled, $ErrorAction)
    @($script:fwRules | Where-Object { -not $DisplayName -or $_.DisplayName -like $DisplayName })
}
function Get-NetFirewallPortFilter { param([Parameter(ValueFromPipeline = $true)]$InputObject) process { [pscustomobject]@{ LocalPort = $InputObject.Port; Protocol = $InputObject.Proto } } }
function New-NetFirewallRule {
    param($DisplayName, $Direction, $Action, $Protocol, $LocalPort, $Profile, $ErrorAction)
    $script:fwRules += [pscustomobject]@{ DisplayName = $DisplayName; Action = $Action; Port = $LocalPort; Proto = $Protocol; Profile = $Profile }
}
function Remove-NetFirewallRule {
    param([Parameter(ValueFromPipeline = $true)]$InputObject)
    process { $script:fwRules = @($script:fwRules | Where-Object { $_.DisplayName -ne $InputObject.DisplayName }) }
}
# GitHub's API gives no answer unless a test stands in for it, so no check
# reaches the network
function Invoke-RestMethod { throw [System.Net.WebException]::new('The remote name could not be resolved: api.github.com') }
# The machine's Path, kept here instead of in the registry
$script:machinePath = 'C:\Windows\system32;C:\Windows'
function Get-MachinePath { $script:machinePath }
function Set-MachinePath([string]$Value) { $script:machinePath = $Value }

function New-TestInstall {
    $dir = Join-Path ([System.IO.Path]::GetTempPath()) ("nd-ps1-" + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path (Join-Path $dir 'server'), (Join-Path $dir 'console') -Force | Out-Null
    $script:RUSTDESK_PATH = Join-Path $dir 'server'
    $script:CONSOLE_PATH = Join-Path $dir 'console'
    $script:ScriptDir = $dir
    $script:SERVER_SERVICE = 'NeoDeskServer'
    $script:CONSOLE_SERVICE = 'NeoDeskConsole'
    $script:HBBS_SERVICE = 'NeoDeskSignal'
    $script:HBBR_SERVICE = 'NeoDeskRelay'
    $script:GO_API_PORT = '21114'
    $script:AUTO_MODE = $true
    $script:RELAY_SERVERS = '203.0.113.7'
    $script:RELAY_MODE = 'auto'
    $script:USE_POSTGRESQL = $false
    $script:RUN_AS_ROOT = $false
    $script:LOG_FILE = Join-Path $dir 'test.log'
    $script:BACKUP_DIR = Join-Path $dir 'backups'
    $script:icaclsCalls = @()
    $script:fwRules = @()
    $env:CONNECTION_MODE_SET = '1'
    $script:registeredTasks = @{}
    $script:fakeService = $null
    $script:scCalls = @()
    $script:nssmSettings = @{}
    $script:leastPrivilege = @()
    return $dir
}

# WD-2: the install makes NeoDeskServer the Windows service that the binary
# is, under its own virtual account (LocalSystem only with -RunAsRoot),
# started with Windows. The service manager starts it again after a crash
# and after exit status 3, the server's restart request (failureflag); the
# server is told so (NEODESK_SUPERVISOR) and reads .env. No scheduled task,
# no launcher, no NSSM, and the name carries no version (WD-17).
function Test-InstallMakesTheWindowsService {
    $dir = New-TestInstall
    Use-RealServerBinaryPath
    $exe = Join-Path $script:RUSTDESK_PATH 'neodesk-server.exe'
    $ok = Setup-Services 6> $null
    $svc = $script:fakeService
    if ("$ok" -ne 'True' -or -not $svc) { Fail "the install made no service: $ok"; Remove-Item -Recurse -Force $dir; return }
    if (-not $svc.PathName.StartsWith("`"$exe`" -mode all ")) { Fail "the service does not run the binary itself: $($svc.PathName)" }
    if ($svc.StartName -ne 'NT SERVICE\NeoDeskServer') { Fail "the service runs as $($svc.StartName)" }
    if ($svc.DisplayName -ne 'NeoDesk Server') { Fail "the service's display name is '$($svc.DisplayName)'" }
    if ($svc.StartupType -ne 'Automatic') { Fail "the service starts $($svc.StartupType)" }
    if ($svc.Environment -notcontains 'NEODESK_SUPERVISOR=windows-service') { Fail "the server is not told the service manager starts it again: $($svc.Environment -join ' ')" }
    if ($svc.Environment -notcontains ('NEODESK_ENV_FILE=' + (Join-Path $script:CONSOLE_PATH '.env'))) { Fail "the service does not name the env file: $($svc.Environment -join ' ')" }
    if ($svc.Environment -contains 'CONSOLE_PORT=0') { Fail 'the service keeps the console off' }
    $sc = @($script:scCalls | ForEach-Object { $_ -join ' ' })
    if ($sc -notcontains 'failure NeoDeskServer reset= 86400 actions= restart/2000/restart/5000/restart/10000') { Fail "the service is not started again after a failure: $($sc -join ' | ')" }
    if ($sc -notcontains 'failureflag NeoDeskServer 1') { Fail "an exit with status 3 does not start the service again: $($sc -join ' | ')" }
    if (@($script:leastPrivilege | Where-Object { $_[0] -eq 'NeoDeskServer' -and ($_[1] -join '|') -eq "$($script:RUSTDESK_PATH)|$($script:CONSOLE_PATH)" }).Count -ne 1) { Fail "the service's account gets no rights on the folders" }
    if (-not (Test-Path (Split-Path -Parent (Get-ServerLogPath)))) { Fail 'the service has no folder for its log' }
    if ($script:registeredTasks.Count -gt 0) { Fail 'the install registered a scheduled task' }
    if (Test-Path (Join-Path $script:RUSTDESK_PATH 'start-neodesk-server.cmd')) { Fail 'the install wrote a task launcher' }
    $out = (Show-InstallSummary 6>&1 | Out-String -Width 4096)
    if ($out -notmatch 'Runs as:\s+Windows service NeoDeskServer, as NT SERVICE\\NeoDeskServer') { Fail "the summary does not say how the server runs: $out" }
    if ($out -notmatch 'then: Restart-Service NeoDeskServer') { Fail "the summary does not restart the service: $out" }
    $text = Get-Content $installer -Raw
    if ($text -match '(?m)^[^#\r\n]*&\s*\$?nssm\b' -or $text -match '(^|[^n])Register-ScheduledTask') { Fail 'neodesk.ps1 still runs nssm or registers a scheduled task' }

    # -RunAsRoot: LocalSystem, and the folders need no grant
    $script:RUN_AS_ROOT = $true
    Setup-Services 6> $null | Out-Null
    $script:RUN_AS_ROOT = $false
    if ($script:fakeService.StartName -ne 'LocalSystem') { Fail "-RunAsRoot runs the service as $($script:fakeService.StartName)" }

    # A binary from before the service is not made one, which the service
    # manager would end after 30 seconds
    $script:fakeService = $null
    function Test-BinaryRunsAsService { param($Path) $false }
    $ok = Setup-Services 6> $null
    if ("$ok" -ne 'False' -or $script:fakeService) { Fail 'an older binary became the service' }
    Remove-Item -Recurse -Force $dir
}

# The binary says it runs as a Windows service with the line it logs on a
# stop of the service; the installer looks for that line.
function Test-BinaryRunsAsServiceProbe {
    $dir = New-TestInstall
    $line = 'Service stop requested, shutting down'
    $main = Get-Content (Join-Path $root 'neodesk-server/main.go') -Raw
    if (-not $main.Contains("`"$line...`"")) { Fail "main.go no longer logs '$line', which the installer looks for" }
    foreach ($fn in $ast.FindAll({ $args[0] -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $args[0].Name -eq 'Test-BinaryRunsAsService' }, $false)) {
        . ([scriptblock]::Create($fn.Extent.Text))
    }
    $probe = Join-Path $dir 'probe.exe'
    Set-Content -Path $probe -Value "bytes $line... bytes"
    if (-not (Test-BinaryRunsAsService -Path $probe)) { Fail 'a binary with the service does not count as one' }
    Set-Content -Path $probe -Value 'bytes Received signal %v, shutting down... bytes'
    if (Test-BinaryRunsAsService -Path $probe) { Fail 'an older binary counts as one that runs as a service' }
    Remove-Item -Recurse -Force $dir
}

# WD-2: an update makes an older install's scheduled task (a SYSTEM task
# whose launcher looped the server) the Windows service, with the task's
# arguments and settings; the task and its launcher go. A launcher written
# while the Node.js console ran held CONSOLE_PORT=0 (BN4).
function Test-TaskBecomesTheService {
    $dir = New-TestInstall
    Use-RealServerBinaryPath
    $launcher = Join-Path $script:RUSTDESK_PATH 'start-neodesk-server.cmd'
    $exe = Join-Path $script:RUSTDESK_PATH 'neodesk-server.exe'
    Set-Content -Path (Join-Path $script:CONSOLE_PATH '.env') -Value @('SESSION_SECRET=kept', 'NEODESK_SUPERVISOR=task')
    Set-Content -Path $launcher -Encoding ASCII -Value @(
        '@echo off',
        'set "DB_URL=C:\NeoDesk\db_v2.sqlite3"',
        'set "CONSOLE_PORT=0"',
        'set "NEODESK_SUPERVISOR=task"',
        ':run',
        "`"$exe`" -mode all -relay-servers 203.0.113.7 -api-port 21114 >> `"C:\NeoDesk\logs\server.log`" 2>&1",
        'goto run'
    )
    $script:registeredTasks['NeoDeskServer'] = 'old'
    $out = (Update-ServerServiceDefinition 6>&1 | Out-String -Width 4096)
    $svc = $script:fakeService
    if (-not $svc) { Fail "the task did not become the service: $out"; Remove-Item -Recurse -Force $dir; return }
    if ($svc.PathName -ne "`"$exe`" -mode all -relay-servers 203.0.113.7 -api-port 21114") { Fail "the service lost the task's arguments: $($svc.PathName)" }
    if ($svc.Environment -notcontains 'DB_URL=C:\NeoDesk\db_v2.sqlite3') { Fail "the service lost the launcher's settings: $($svc.Environment -join ' ')" }
    if ($svc.Environment -contains 'CONSOLE_PORT=0') { Fail 'the service keeps CONSOLE_PORT=0 of the old launcher' }
    if (@($svc.Environment | Where-Object { $_ -like 'NEODESK_SUPERVISOR=*' }) -join ' ' -ne 'NEODESK_SUPERVISOR=windows-service') { Fail "the service's supervisor: $($svc.Environment -join ' ')" }
    if ($svc.StartName -ne 'NT SERVICE\NeoDeskServer') { Fail "the former task runs as $($svc.StartName)" }
    if ($script:registeredTasks.ContainsKey('NeoDeskServer')) { Fail 'the task stays next to the service and would start a second server' }
    if (Test-Path $launcher) { Fail 'the launcher stays' }
    if ((Get-ConsoleEnvValue -Key 'NEODESK_SUPERVISOR') -ne 'windows-service') { Fail ".env still names the task: $(Get-ConsoleEnvValue -Key 'NEODESK_SUPERVISOR')" }
    if ($out -notmatch 'scheduled task NeoDeskServer is now a Windows service') { Fail "the update does not say the task became the service: $out" }
    Remove-Item -Recurse -Force $dir
}

# WD-2: an update makes an older install's NSSM service the binary's own
# service in place, with NSSM's arguments and environment read from the
# registry (nssm.exe need not be there); its NSSM settings go and
# -tls-api is dropped as before.
function Test-NssmServiceBecomesTheService {
    $dir = New-TestInstall
    Use-RealServerBinaryPath
    $exe = Join-Path $script:RUSTDESK_PATH 'neodesk-server.exe'
    $script:fakeService = [pscustomobject]@{ PathName = 'C:\NeoDesk\tools\nssm.exe'; StartName = 'NT SERVICE\NeoDeskServer'; DisplayName = 'NeoDesk Server 3.6.20'; StartupType = 'Automatic'; Environment = @() }
    $script:nssmSettings = @{
        AppParameters       = '-mode all -relay-servers 203.0.113.7 -api-port 21114 -tls-api'
        AppEnvironmentExtra = @('NEODESK_SUPERVISOR=nssm', 'P2P_FIRST=Y', 'SIGNAL_PORT=31116')
    }
    Set-Content -Path (Join-Path $script:CONSOLE_PATH '.env') -Value 'SESSION_SECRET=kept'
    if ((Get-ServerSupervisor) -ne 'nssm') { Fail "an NSSM service counts as $(Get-ServerSupervisor)" }
    Update-ServerServiceDefinition 6> $null | Out-Null
    $svc = $script:fakeService
    if ($svc.PathName -ne "`"$exe`" -mode all -relay-servers 203.0.113.7 -api-port 21114") { Fail "the service runs $($svc.PathName)" }
    if ((Get-ServerSupervisor) -ne 'service') { Fail "after the update the service counts as $(Get-ServerSupervisor)" }
    if ($svc.Environment -notcontains 'P2P_FIRST=Y' -or $svc.Environment -notcontains 'NEODESK_SUPERVISOR=windows-service') { Fail "the service's settings: $($svc.Environment -join ' ')" }
    if (@($svc.Environment | Where-Object { $_ -like 'SIGNAL_PORT=*' }).Count -gt 0 -or (Get-ConsoleEnvValue -Key 'SIGNAL_PORT') -ne '31116') { Fail "the NSSM service's port did not move to .env" }
    if ($script:nssmSettings.Count -gt 0) { Fail "the NSSM settings stay: $($script:nssmSettings.Keys -join ', ')" }
    if ($svc.DisplayName -ne 'NeoDesk Server') { Fail "the display name keeps a version: $($svc.DisplayName)" }
    Remove-Item -Recurse -Force $dir
}

# The SSL menu and the protocol switch change TLS on the service's command
# line, with paths in quotes, and take it off again; nothing else of the
# command line changes.
function Test-TlsOnTheServiceCommandLine {
    $dir = New-TestInstall
    Use-RealServerBinaryPath
    $exe = Join-Path $script:RUSTDESK_PATH 'neodesk-server.exe'
    $script:fakeService = [pscustomobject]@{ PathName = "`"$exe`" -mode all -relay-servers 203.0.113.7 -api-port 21114 -tls-api"; StartName = 'NT SERVICE\NeoDeskServer'; DisplayName = 'NeoDesk Server'; StartupType = 'Automatic'; Environment = @('NEODESK_SUPERVISOR=windows-service') }
    Set-ServerTls -CertPath 'C:\Neo Desk\ssl\neodesk.crt' -KeyPath 'C:\Neo Desk\ssl\neodesk.key'
    $want = "`"$exe`" -mode all -relay-servers 203.0.113.7 -api-port 21114 -tls-cert `"C:\Neo Desk\ssl\neodesk.crt`" -tls-key `"C:\Neo Desk\ssl\neodesk.key`" -tls-signal -tls-relay"
    if ($script:fakeService.PathName -ne $want) { Fail "TLS on the command line: $($script:fakeService.PathName)" }
    Set-ServerTls -CertPath 'C:\NeoDesk\ssl\other.crt' -KeyPath 'C:\NeoDesk\ssl\other.key'
    if (([regex]::Matches($script:fakeService.PathName, '-tls-cert')).Count -ne 1 -or $script:fakeService.PathName -notmatch [regex]::Escape('-tls-cert "C:\NeoDesk\ssl\other.crt"')) { Fail "a second certificate: $($script:fakeService.PathName)" }
    Set-ServerTls
    if ($script:fakeService.PathName -ne "`"$exe`" -mode all -relay-servers 203.0.113.7 -api-port 21114") { Fail "TLS off: $($script:fakeService.PathName)" }
    if ((Split-ServiceCommandLine -PathName 'C:\NeoDesk\neodesk-server.exe -mode all') -ne '-mode all') { Fail 'an unquoted command line loses its arguments' }

    # The protocol switch: HTTPS keys of an older service go, .env has them
    $script:fakeService.Environment = @('NEODESK_SUPERVISOR=windows-service', 'HTTPS_ENABLED=true', 'NEODESK_API_URL=https://localhost:21114/api', 'P2P_FIRST=Y')
    Clear-TlsSettingsOfService
    if (($script:fakeService.Environment -join ' ') -ne 'NEODESK_SUPERVISOR=windows-service NEODESK_API_URL=http://localhost:21114/api P2P_FIRST=Y') { Fail "the switch left the service's settings as $($script:fakeService.Environment -join ' ')" }
    Remove-Item -Recurse -Force $dir
}

# The console's language files, themes and images come from the binary:
# an update copies none from a web-nodejs folder an older release left in
# the checkout, and the console folder keeps its .env, data, installed
# fonts and language files.
function Test-ConsoleHomeCopiesNoConsoleFiles {
    $dir = New-TestInstall
    $old = Join-Path $dir 'web-nodejs'
    $console = $script:CONSOLE_PATH
    New-Item -ItemType Directory -Force -Path (Join-Path $old 'lang'), (Join-Path $old 'themes'), `
        (Join-Path $console 'data\transfers'), (Join-Path $console 'public\fonts'), (Join-Path $console 'lang') | Out-Null
    Set-Content -Path (Join-Path $old 'lang\en.json') -Value '{"old":true}'
    Set-Content -Path (Join-Path $old 'themes\dark.css') -Value 'old'
    Set-Content -Path (Join-Path $console '.env') -Value 'SESSION_SECRET=kept'
    Set-Content -Path (Join-Path $console 'data\transfers\a.bin') -Value 'file'
    Set-Content -Path (Join-Path $console 'public\fonts\inter.woff2') -Value 'font'
    Set-Content -Path (Join-Path $console 'lang\xx.json') -Value '{"custom":true}'
    $script:MINIMAL_MODE = $false
    $script:mergedFresh = $null
    function Merge-ConsoleEnv { param([bool]$FreshInstall) $script:mergedFresh = $FreshInstall; return $true }
    Setup-ConsoleHome | Out-Null
    if ($script:mergedFresh -ne $false) { Fail "an install with .env counted as fresh ($($script:mergedFresh))" }
    if (Test-Path (Join-Path $console 'lang\en.json')) { Fail "the update copied the checkout's web-nodejs\lang into the console folder" }
    if (Test-Path (Join-Path $console 'themes')) { Fail "the update copied the checkout's web-nodejs\themes into the console folder" }
    if ((Get-Content (Join-Path $console '.env') -Raw).Trim() -ne 'SESSION_SECRET=kept') { Fail 'the update changed .env' }
    if (-not (Test-Path (Join-Path $console 'data\transfers\a.bin'))) { Fail 'the update lost a file of data' }
    if (-not (Test-Path (Join-Path $console 'public\fonts\inter.woff2'))) { Fail 'the update lost an installed font' }
    if (-not (Test-Path (Join-Path $console 'lang\xx.json'))) { Fail 'the update lost a language file of the console folder' }
    Remove-Item -Recurse -Force $dir
}

# RV83: the migrate tool replaces an existing SQLite destination only with
# -overwrite. The menus pass it after the user agreed, and do not run the
# migration when the user declined.
function Test-MigrationMenusPassOverwrite {
    $dir = New-TestInstall
    $script:AUTO_MODE = $false
    $script:migrateCalls = @()
    function Get-MigrateBin { return 'fake-migrate' }
    function fake-migrate { $script:migrateCalls += ($args -join ' '); $global:LASTEXITCODE = 0 }
    function Invoke-MenuChoose { param($Title, $Subtitle, $Items, $Returns) $script:MENU_CHOICE = $script:testChoice; $script:menuItems = $Items }
    function Read-Host { param($Prompt) if ($script:answers.Count -gt 0) { $script:answers.Dequeue() } else { '' } }
    function Press-Enter { }
    function Clear-Host { }
    function Invoke-TestMenu([string]$Choice, [string[]]$Answers) {
        $script:migrateCalls = @()
        $script:testChoice = $Choice
        $script:answers = New-Object System.Collections.Queue
        foreach ($a in $Answers) { $script:answers.Enqueue($a) }
        Do-MigrateDatabase *> $null
        return @($script:migrateCalls | Where-Object { $_ -match '-mode (rust2go|pg2sqlite|sqlite2pg)' })
    }

    $src = Join-Path $script:RUSTDESK_PATH 'db_v2.sqlite3'
    $dst = Join-Path $script:RUSTDESK_PATH 'neodesk.sqlite3'
    Set-Content -Path $src -Value 'db'
    Set-Content -Path $dst -Value 'old'

    # RV81: sqlite2pg and pg2sqlite move the whole install, and the menu
    # says so. sqlite2pg replaces a PostgreSQL database with data in it only
    # after the user agreed.
    $runs = Invoke-TestMenu '2' @('', 'postgres://u@h/db', 'y')
    $offered = (@($script:menuItems | ForEach-Object { ($_ -split "`t")[0] }) -join '|')
    if ($offered -ne 'Rust -> Go|SQLite -> PostgreSQL|PostgreSQL -> SQLite|Backup|Back') { Fail "the migration menu offers: $offered" }
    if (-not ($script:menuItems -match '^SQLite -> PostgreSQL\tWhole install')) { Fail "the menu does not say SQLite -> PostgreSQL moves the whole install: $($script:menuItems)" }
    if (-not ($script:menuItems -match '^PostgreSQL -> SQLite\tWhole install')) { Fail "the menu does not say PostgreSQL -> SQLite moves the whole install: $($script:menuItems)" }
    if ("$runs" -ne "-mode sqlite2pg -src $src -dst postgres://u@h/db -overwrite") { Fail "sqlite2pg after an agreed replace ran as: $runs" }
    $runs = Invoke-TestMenu '2' @('', 'postgres://u@h/db', 'n')
    if ("$runs" -ne "-mode sqlite2pg -src $src -dst postgres://u@h/db") { Fail "sqlite2pg after a declined replace ran as: $runs" }

    # L72: a move of the install's own database points .env at the new one
    # and restarts the server
    $envFile = Join-Path $script:CONSOLE_PATH '.env'
    Set-Content -Path $envFile -Value @('SESSION_SECRET=kept', 'DB_TYPE=sqlite', 'DATABASE_URL=', "DB_URL=$src")
    $script:restarted = $false
    function script:Stop-ServerService { }
    function script:Start-ServerService { $script:restarted = $true }
    Invoke-TestMenu '2' @('', 'postgres://u@h/db', 'n') | Out-Null
    if ((Get-ConsoleEnvValue -Key 'DB_TYPE') -ne 'postgres' -or (Get-ConsoleEnvValue -Key 'DATABASE_URL') -ne 'postgres://u@h/db' -or (Get-ConsoleEnvValue -Key 'DB_URL') -ne 'postgres://u@h/db') {
        Fail "after the menu's move .env does not point at PostgreSQL: $(Get-Content $envFile)"
    }
    if (-not $script:restarted) { Fail 'the server was not started again after the move' }
    Invoke-TestMenu '3' @('postgres://u@h/db', '', 'y') | Out-Null
    if ((Get-ConsoleEnvValue -Key 'DB_TYPE') -ne 'sqlite' -or (Get-ConsoleEnvValue -Key 'DB_URL') -ne $src) { Fail "after the menu's move back .env does not point at SQLite: $(Get-Content $envFile)" }
    Remove-Item -Force $envFile

    $runs = Invoke-TestMenu '3' @('postgres://u@h/db', '', 'y')
    if ($runs -notcontains "-mode pg2sqlite -src postgres://u@h/db -dst $src -overwrite") { Fail "pg2sqlite after an agreed overwrite ran as: $runs" }
    $runs = Invoke-TestMenu '3' @('postgres://u@h/db', '', 'n')
    if ($runs.Count -ne 0) { Fail "pg2sqlite ran although the overwrite was declined: $runs" }

    $runs = Invoke-TestMenu '1' @('', '', 'y')
    if ($runs -notcontains "-mode rust2go -src $src -dst $dst -overwrite") { Fail "rust2go after an agreed overwrite ran as: $runs" }
    $runs = Invoke-TestMenu '1' @('', '', 'n')
    if ($runs.Count -ne 0) { Fail "rust2go ran although the overwrite was declined: $runs" }

    Remove-Item -Force $dst
    $runs = Invoke-TestMenu '1' @('', '')
    if ($runs -notcontains "-mode rust2go -src $src -dst $dst") { Fail "rust2go into a new file ran as: $runs" }
    $runs = Invoke-TestMenu '1' @('', 'postgres://u@h/db')
    if ($runs -notcontains "-mode rust2go -src $src -dst postgres://u@h/db") { Fail "rust2go to PostgreSQL ran as: $runs" }
    Remove-Item -Recurse -Force $dir
}

# RV81: the install's switch to PostgreSQL moves the whole SQLite install
# with sqlite2pg, passes -overwrite only after the user agreed (the
# installer does not create the database on Windows), and points .env at
# PostgreSQL only after the move succeeded; when it fails the install stays
# on its SQLite file.
function Test-PostgreSQLSwitchMovesTheWholeInstall {
    $dir = New-TestInstall
    $script:migrateCalls = @()
    $script:migrateExit = 0
    function Get-MigrateBin { return 'fake-migrate' }
    function fake-migrate {
        $script:migrateCalls += ($args -join ' ')
        $global:LASTEXITCODE = if ($args[1] -eq 'sqlite2pg') { $script:migrateExit } else { 0 }
    }
    function Read-Host { param($Prompt) if ($script:answers.Count -gt 0) { $script:answers.Dequeue() } else { '' } }
    $src = Join-Path $script:RUSTDESK_PATH 'db_v2.sqlite3'
    $envFile = Join-Path $script:CONSOLE_PATH '.env'
    $uri = 'postgres://u:p@h/db?sslmode=disable'
    Set-Content -Path $src -Value 'db'
    function Invoke-Switch([int]$Exit, [bool]$Auto, [string[]]$Answers) {
        $script:migrateCalls = @()
        $script:migrateExit = $Exit
        $script:answers = New-Object System.Collections.Queue
        foreach ($a in $Answers) { $script:answers.Enqueue($a) }
        Set-Content -Path $envFile -Value @('SESSION_SECRET=kept', 'DB_TYPE=postgres', "DATABASE_URL=$uri", "DB_URL=$uri")
        $script:USE_POSTGRESQL = $true
        $script:POSTGRESQL_URI = $uri
        $script:AUTO_MODE = $Auto
        Migrate-SQLiteToPostgreSQL *> $null
        return @($script:migrateCalls | Where-Object { $_ -match '-mode sqlite2pg' })
    }

    $runs = Invoke-Switch 0 $true @()
    if ("$runs" -ne "-mode sqlite2pg -src $src -dst $uri") { Fail "the unattended switch ran as: $runs" }
    if (-not ($script:migrateCalls -contains "-mode backup -src $src")) { Fail "the switch made no backup first: $($script:migrateCalls)" }
    if (-not $script:USE_POSTGRESQL) { Fail 'after a move the install does not use PostgreSQL' }
    if ((Get-ConsoleEnvValue -Key 'DB_TYPE') -ne 'postgres' -or (Get-ConsoleEnvValue -Key 'DB_URL') -ne $uri -or (Get-ConsoleEnvValue -Key 'DATABASE_URL') -ne $uri) {
        Fail "after a move .env does not point at PostgreSQL: $(Get-Content $envFile)"
    }
    if ((Get-ConsoleEnvValue -Key 'SESSION_SECRET') -ne 'kept') { Fail 'the switch lost a line of .env' }

    $runs = Invoke-Switch 0 $false @('y', 'y')
    if ("$runs" -ne "-mode sqlite2pg -src $src -dst $uri -overwrite") { Fail "the switch after an agreed replace ran as: $runs" }
    $runs = Invoke-Switch 0 $false @('y', 'n')
    if ("$runs" -ne "-mode sqlite2pg -src $src -dst $uri") { Fail "the switch after a declined replace ran as: $runs" }

    # L60: a declined move keeps the install on SQLite
    $runs = Invoke-Switch 0 $false @('n')
    if ($runs.Count -ne 0) { Fail "the switch moved the data although the move was declined: $runs" }
    if ($script:USE_POSTGRESQL -or $script:POSTGRESQL_URI) { Fail "after a declined move the install still uses PostgreSQL ($($script:USE_POSTGRESQL), $($script:POSTGRESQL_URI))" }
    if ((Get-ConsoleEnvValue -Key 'DB_TYPE') -ne 'sqlite' -or (Get-ConsoleEnvValue -Key 'DB_URL') -ne $src -or (Get-ConsoleEnvValue -Key 'DATABASE_URL') -ne '') {
        Fail "after a declined move .env does not point at the SQLite file: $(Get-Content $envFile)"
    }

    $runs = Invoke-Switch 1 $true @()
    if ($script:USE_POSTGRESQL -or $script:POSTGRESQL_URI) { Fail "after a failed move the install still uses PostgreSQL ($($script:USE_POSTGRESQL), $($script:POSTGRESQL_URI))" }
    if ((Get-ConsoleEnvValue -Key 'DB_TYPE') -ne 'sqlite' -or (Get-ConsoleEnvValue -Key 'DB_URL') -ne $src -or (Get-ConsoleEnvValue -Key 'DATABASE_URL') -ne '') {
        Fail "after a failed move .env does not point at the SQLite file: $(Get-Content $envFile)"
    }
    $script:AUTO_MODE = $true
    Remove-Item -Recurse -Force $dir
}

# L31: releases come from the fork's repository. The defaults are set at
# the script's top level, so those lines are run here as they are.
function Test-ReleasesComeFromTheFork {
    $saved = @{}
    foreach ($k in 'UPDATE_GITHUB_OWNER', 'UPDATE_GITHUB_REPO', 'UPDATE_GITHUB_BRANCH', 'RELEASE_BASE_URL') {
        $saved[$k] = [Environment]::GetEnvironmentVariable($k)
        [Environment]::SetEnvironmentVariable($k, $null)
    }
    $defaults = $ast.FindAll({
        $args[0] -is [System.Management.Automation.Language.AssignmentStatementAst] -and
        $args[0].Left.Extent.Text -like '$script:UPDATE_GITHUB_*'
    }, $false)
    if ($defaults.Count -lt 2) { Fail "the installer sets no repository for releases ($($defaults.Count) lines)" }
    foreach ($a in $defaults) { . ([scriptblock]::Create($a.Extent.Text)) }
    $base = Get-ReleaseBaseUrl
    if ($base -ne 'https://github.com/neodeskapp/neodesk/releases/latest/download') { Fail "the stable channel downloads from $base" }
    $script:UPDATE_GITHUB_BRANCH = 'dev'
    $base = Get-ReleaseBaseUrl
    if ($base -ne 'https://github.com/neodeskapp/neodesk/releases/download/dev') { Fail "the development channel downloads from $base" }
    $script:UPDATE_GITHUB_BRANCH = 'main'
    foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k]) }
}

# A stand-in for neodesk-server.exe: env merge writes a small .env (the
# admin password only on a fresh one), upgrade-env changes nothing.
function Use-FakeServerBinary {
    function script:Get-ServerBinaryPath { 'fake-server' }
    function script:Test-BinaryHasSubcommands { param($Path) $true }
    function script:fake-server {
        if ($args[0] -eq 'env' -and $args[1] -eq 'merge') {
            $target = $args[3]
            if ($args -contains '--fresh') {
                Set-Content -Path $target -Value @("DEFAULT_ADMIN_PASSWORD=$env:ND_SUBST_DEFAULT_ADMIN_PASSWORD", 'DB_TYPE=sqlite')
            }
            Write-Output '{"success":true}'
        }
        $global:LASTEXITCODE = 0
    }
}

# L65 and L36: a fresh install starts in Managed through .env, the first
# default a mode saved in the console replaces. An update adds no mode,
# and the service gets neither the mode nor ENROLLMENT_MODE_ENV_OVERRIDE,
# which would put the installer's value over the console's.
function Test-FreshInstallStartsManaged {
    $dir = New-TestInstall
    Use-FakeServerBinary
    $envFile = Join-Path $script:CONSOLE_PATH '.env'
    $env:ADMIN_PASSWORD = 'Fresh-Admin-Pass-1'
    Merge-ConsoleEnv -FreshInstall $true *> $null
    if ((Get-ConsoleEnvValue -Key 'ENROLLMENT_MODE') -ne 'managed') { Fail "a fresh .env does not start in Managed: $(Get-Content $envFile)" }
    Set-Content -Path $envFile -Value @('SESSION_SECRET=kept', 'DB_TYPE=sqlite')
    Merge-ConsoleEnv -FreshInstall $false *> $null
    if (Get-ConsoleEnvValue -Key 'ENROLLMENT_MODE') { Fail 'an update added an enrollment mode to .env' }
    $env:ADMIN_PASSWORD = $null

    Setup-Services 6> $null | Out-Null
    $envCall = if ($script:fakeService) { @($script:fakeService.Environment) } else { @() }
    if ($envCall.Count -eq 0) { Fail 'the service gets no environment' }
    elseif (@($envCall | Where-Object { $_ -like 'ENROLLMENT_MODE*' }).Count -gt 0) { Fail "the service sets the enrollment mode: $($envCall -join ' ')" }
    Remove-Item -Recurse -Force $dir
}

# L124: Windows does not create the PostgreSQL database, so an unattended
# install takes the address it is given as it is, and without one it needs
# the password of the existing database instead of making one up.
function Test-PostgreSQLAddressIsKept {
    $dir = New-TestInstall
    $script:POSTGRESQL_USER, $script:POSTGRESQL_HOST, $script:POSTGRESQL_PORT, $script:POSTGRESQL_DB = 'neodesk', 'localhost', '5432', 'neodesk'
    $script:USE_POSTGRESQL = $true
    $script:POSTGRESQL_URI = 'postgres://bd:Given-Pass@db.example:5433/desk?sslmode=require'
    $script:POSTGRESQL_PASS = ''
    $ok = Setup-PostgreSQLDatabase 6> $null
    if (-not $ok) { Fail 'the PostgreSQL set-up refused a given address' }
    if ($script:POSTGRESQL_URI -ne 'postgres://bd:Given-Pass@db.example:5433/desk?sslmode=require') { Fail "the given address was replaced: $($script:POSTGRESQL_URI)" }

    $script:POSTGRESQL_URI = ''
    $ok = Setup-PostgreSQLDatabase 6> $null
    if ($ok) { Fail "an unattended install without an address or password went on with $($script:POSTGRESQL_URI)" }

    $script:POSTGRESQL_PASS = 'p@ss/word'
    $ok = Setup-PostgreSQLDatabase 6> $null
    if (-not $ok -or $script:POSTGRESQL_URI -ne 'postgres://neodesk:p%40ss%2Fword@localhost:5432/neodesk?sslmode=disable') { Fail "the address from the parts is $($script:POSTGRESQL_URI)" }
    $script:USE_POSTGRESQL = $false
    $script:POSTGRESQL_URI = ''
    $script:POSTGRESQL_PASS = ''
    Remove-Item -Recurse -Force $dir
}

# L225: -PgUri means PostgreSQL as its help says, also without
# -PostgreSQL, and an interactive install asks no database question for
# it, where the menu's default made it SQLite again.
function Test-PgUriMeansPostgreSQL {
    $dir = New-TestInstall
    $saved = $env:USE_POSTGRESQL
    $env:USE_POSTGRESQL = $null
    $uri = 'postgres://bd:Given-Pass@db.example:5433/desk?sslmode=require'
    if (-not (Test-PostgreSQLWanted -Switch $false -Uri $uri)) { Fail '-PgUri alone does not mean PostgreSQL' }
    if (Test-PostgreSQLWanted -Switch $false -Uri '') { Fail 'neither -PostgreSQL nor -PgUri means PostgreSQL' }
    if (-not (Test-PostgreSQLWanted -Switch $true -Uri '')) { Fail '-PostgreSQL does not mean PostgreSQL' }
    $script:menuShown = $false
    function Invoke-MenuChoose { param($Title, $Subtitle, $Items, $Returns) $script:menuShown = $true; $script:MENU_CHOICE = '1' }
    $script:AUTO_MODE = $false
    $script:USE_POSTGRESQL = $true
    $script:POSTGRESQL_URI = $uri
    Choose-DatabaseType 6> $null
    if ($script:menuShown) { Fail 'the database question was asked with an address given' }
    if (-not $script:USE_POSTGRESQL -or $script:POSTGRESQL_URI -ne $uri) { Fail "the given address did not stay: $($script:USE_POSTGRESQL) $($script:POSTGRESQL_URI)" }
    $script:AUTO_MODE = $true
    $script:USE_POSTGRESQL = $false
    $script:POSTGRESQL_URI = ''
    $env:USE_POSTGRESQL = $saved
    Remove-Item -Recurse -Force $dir
}


# L68: the signal and relay ports live in .env, where the admin changes
# them. The service does not set them, and an update moves the ports an
# older service set into .env instead of setting 21116 and 21117 again.
function Test-UpdateKeepsTheAdminsPorts {
    $dir = New-TestInstall
    $envFile = Join-Path $script:CONSOLE_PATH '.env'
    Set-Content -Path $envFile -Value @('SESSION_SECRET=kept', 'SIGNAL_PORT=21116', 'RELAY_PORT=21117')
    Setup-Services 6> $null | Out-Null
    if (($script:fakeService.Environment -join ' ') -match '(SIGNAL|RELAY)_PORT') { Fail "the service sets the ports: $($script:fakeService.Environment -join ' ')" }
    $script:fakeService.Environment = @('NEODESK_SUPERVISOR=windows-service', 'SIGNAL_PORT=31116', 'RELAY_PORT=31117')
    Update-ServerServiceDefinition 6> $null | Out-Null
    if (($script:fakeService.Environment -join ' ') -match '(SIGNAL|RELAY)_PORT') { Fail "the updated service sets the ports: $($script:fakeService.Environment -join ' ')" }
    if ((Get-ConsoleEnvValue -Key 'SIGNAL_PORT') -ne '31116' -or (Get-ConsoleEnvValue -Key 'RELAY_PORT') -ne '31117') { Fail "the update lost the service's ports: $(Get-Content $envFile)" }
    if ((Get-ConsoleEnvValue -Key 'SESSION_SECRET') -ne 'kept') { Fail 'the update lost a line of .env' }
    Remove-Item -Recurse -Force $dir
}

# L140: -Channel picks the channel, wins over .env and goes into it, and a
# failed download says what to do: no stable release yet, or no way out.
function Test-ChannelAndDownloadErrors {
    $dir = New-TestInstall
    $saved = $env:UPDATE_GITHUB_BRANCH
    $env:UPDATE_GITHUB_BRANCH = $null
    $Channel = 'dev'
    $defaults = $ast.FindAll({
        $args[0] -is [System.Management.Automation.Language.AssignmentStatementAst] -and
        $args[0].Left.Extent.Text -like '$script:UPDATE_GITHUB_*'
    }, $false)
    foreach ($a in $defaults) { . ([scriptblock]::Create($a.Extent.Text)) }
    $envFile = Join-Path $script:CONSOLE_PATH '.env'
    Set-Content -Path $envFile -Value @('UPDATE_GITHUB_BRANCH=main')
    Read-UpdateGitHubBranchFromEnv
    if ($script:UPDATE_GITHUB_BRANCH -ne 'dev') { Fail "-Channel dev lost to the main of .env: $($script:UPDATE_GITHUB_BRANCH)" }
    Use-FakeServerBinary
    Merge-ConsoleEnv -FreshInstall $false *> $null
    if ((Get-ConsoleEnvValue -Key 'UPDATE_GITHUB_BRANCH') -ne 'dev') { Fail "the given channel is not in .env: $(Get-Content $envFile)" }

    $script:UPDATE_GITHUB_BRANCH_GIVEN = ''
    $script:UPDATE_GITHUB_BRANCH = 'main'
    Remove-Item -Force $envFile
    function script:Invoke-WebRequest { throw [System.Net.WebException]::new('The remote server returned an error: (404) Not Found.') }
    $out = (Get-ReleaseBinary -Dir $dir 6>&1 | Out-String -Width 4096)
    if ($out -notmatch 'no stable release' -or $out -notmatch '-Channel dev' -or $out -notmatch '-Binary') { Fail "a missing stable release says: $out" }
    # Without a channel given or saved, no stable release yet: the
    # development build, its channel kept; a given channel gets the error
    if ($out -notmatch 'installing the development build' -or $out -notmatch '-Update -Channel stable') { Fail "no stable release does not fall back to the development build: $out" }
    if ($script:UPDATE_GITHUB_BRANCH -ne 'dev' -or $script:UPDATE_GITHUB_BRANCH_GIVEN -ne 'dev') { Fail "the fallback does not keep the development channel: $($script:UPDATE_GITHUB_BRANCH)" }
    $script:UPDATE_GITHUB_BRANCH_GIVEN = 'main'
    $script:UPDATE_GITHUB_BRANCH = 'main'
    $out = (Get-ReleaseBinary -Dir $dir 6>&1 | Out-String -Width 4096)
    if ($out -match 'installing the development build' -or $out -notmatch 'no stable release yet') { Fail "-Channel stable without a release says: $out" }
    $script:UPDATE_GITHUB_BRANCH_GIVEN = ''
    $script:UPDATE_GITHUB_BRANCH = 'main'
    function script:Invoke-WebRequest { throw [System.Net.WebException]::new('The remote name could not be resolved: github.com') }
    $out = (Get-ReleaseBinary -Dir $dir 6>&1 | Out-String -Width 4096)
    if ($out -notmatch 'Could not reach' -or $out -notmatch '-Binary') { Fail "a download without network says: $out" }
    Remove-Item function:Invoke-WebRequest -ErrorAction SilentlyContinue
    $script:UPDATE_GITHUB_BRANCH = 'main'
    $env:UPDATE_GITHUB_BRANCH = $saved
    Remove-Item -Recurse -Force $dir
}

# L69: the host clients get becomes PANEL_PUBLIC_HOST, so the HTTP port
# redirects to HTTPS there; a value the admin set stays.
function Test-PanelPublicHost {
    $dir = New-TestInstall
    $envFile = Join-Path $script:CONSOLE_PATH '.env'
    Set-Content -Path $envFile -Value @('SESSION_SECRET=kept', 'PANEL_PUBLIC_HOST=')
    Setup-Services | Out-Null
    if ((Get-ConsoleEnvValue -Key 'PANEL_PUBLIC_HOST') -ne '203.0.113.7') { Fail "the install set PANEL_PUBLIC_HOST to '$(Get-ConsoleEnvValue -Key 'PANEL_PUBLIC_HOST')'" }
    Set-PanelPublicHost -Address '198.51.100.1:21117'
    if ((Get-ConsoleEnvValue -Key 'PANEL_PUBLIC_HOST') -ne '203.0.113.7') { Fail 'a second address replaced PANEL_PUBLIC_HOST' }
    Set-Content -Path $envFile -Value @('SESSION_SECRET=kept')
    Set-PanelPublicHost -Address 'desk.example.org:21117'
    if ((Get-ConsoleEnvValue -Key 'PANEL_PUBLIC_HOST') -ne 'desk.example.org') { Fail "PANEL_PUBLIC_HOST from host:port is '$(Get-ConsoleEnvValue -Key 'PANEL_PUBLIC_HOST')'" }
    Remove-Item -Recurse -Force $dir
}

# L151: -Help documents every parameter; a second -Auto run updates; the
# password shows only when the install made it and leaves .env after the
# first start; the install keeps a copy of the script for updates; the
# summary names the log and the commands for later.
function Test-WindowsFirstHour {
    $documented = @([regex]::Matches((Get-Content $installer -Raw), '(?m)^\.PARAMETER\s+(\w+)') | ForEach-Object { $_.Groups[1].Value })
    foreach ($p in $ast.ParamBlock.Parameters) {
        $name = $p.Name.VariablePath.UserPath
        if ($name -in 'NodeJs', 'Flask') { continue }
        if ($documented -notcontains $name) { Fail "the help does not document -$name" }
    }
    if (-not ($ast.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -eq 'Help' })) { Fail 'there is no -Help' }

    # The real binary path again (Use-FakeServerBinary replaced it)
    foreach ($fn in $ast.FindAll({ $args[0] -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $args[0].Name -eq 'Get-ServerBinaryPath' }, $false)) {
        . ([scriptblock]::Create(($fn.Extent.Text -replace '^function ', 'function script:')))
    }
    $dir = New-TestInstall
    $script:UNINSTALL_MODE = $false
    $script:MINIMAL_MODE = $false
    if ((Get-AutoAction) -ne 'install') { Fail "a machine without NeoDesk gets $(Get-AutoAction)" }
    Setup-Services | Out-Null
    Set-Content -Path (Join-Path $script:RUSTDESK_PATH 'neodesk-server.exe') -Value 'exe'
    $envFile = Join-Path $script:CONSOLE_PATH '.env'
    Set-Content -Path $envFile -Value @('DEFAULT_ADMIN_PASSWORD=First-Pass-1', 'INIT_ADMIN_PASS=First-Pass-1', 'SESSION_SECRET=kept', 'DB_TYPE=sqlite')
    Copy-Item $envFile "$envFile.pre-go"
    if ((Get-AutoAction) -ne 'update') { Fail "a second run on an install gets $(Get-AutoAction)" }
    $script:USE_POSTGRESQL = $true
    if ((Get-AutoAction) -ne 'install') { Fail "-PostgreSQL on a SQLite install gets $(Get-AutoAction), not the move" }
    $script:USE_POSTGRESQL = $false

    $env:ADMIN_PASSWORD = $null
    $script:FRESH_CONSOLE_ENV = $false
    $got = Create-AdminUser 6> $null
    if ($got) { Fail 'a run that kept .env has an admin password to show' }
    $script:FRESH_CONSOLE_ENV = $true
    $shown = @()
    $got = Create-AdminUser 6>&1 | ForEach-Object { if ($_ -is [System.Management.Automation.InformationRecord]) { $shown += "$_" } else { $_ } }
    if ($got -ne 'First-Pass-1') { Fail "a fresh install has no admin password for the summary: $got" }
    # WD-3: the summary at the end shows it, not a step in the middle
    if (($shown -join "`n") -match 'First-Pass-1') { Fail 'the admin step shows the password in the middle of the install' }
    $out = (Show-InstallSummary -AdminPassword $got 6>&1 | Out-String)
    if ($out -notmatch 'First-Pass-1') { Fail 'the summary of a fresh install does not show the admin password' }
    $out = (Show-InstallSummary 6>&1 | Out-String -Width 4096)
    if ($out -match 'First-Pass-1') { Fail 'the summary shows a password it was not given' }
    Remove-FirstAdminPassword
    foreach ($f in $envFile, "$envFile.pre-go") {
        if ((Get-Content $f -Raw) -match 'First-Pass-1') { Fail "$(Split-Path -Leaf $f) keeps the admin password after the first start" }
    }
    if ((Get-ConsoleEnvValue -Key 'SESSION_SECRET') -ne 'kept') { Fail 'forgetting the password lost a line of .env' }

    $script:SELF_PATH = $installer
    Install-ManagerScript 6> $null
    $copy = Join-Path $script:RUSTDESK_PATH 'neodesk.ps1'
    if (-not (Test-Path $copy) -or (Get-FileHash $copy).Hash -ne (Get-FileHash $installer).Hash) { Fail "the install keeps no copy of the script at $copy" }

    Set-Content -Path (Join-Path $script:RUSTDESK_PATH 'id_ed25519.pub') -Value 'AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJ='
    Set-ConsoleEnvValue -Key 'PANEL_PUBLIC_HOST' -Value 'desk.example.org'
    $out = (Show-InstallSummary 6>&1 | Out-String -Width 4096)
    Set-ConsoleEnvValue -Key 'API_PORT' -Value '21121'
    $out = (Show-InstallSummary 6>&1 | Out-String -Width 4096)
    if (-not $out.Contains('http://desk.example.org:21121')) { Fail "the summary gives clients another API address (L76): $out" }
    foreach ($want in 'desk.example.org', 'AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJ=', (Get-ServerLogPath), $envFile, '-Update', '-Uninstall', '-ResetPassword', 'admin') {
        if (-not $out.Contains($want)) { Fail "the summary does not name $want" }
    }
    # WD-6: the commands for later run in a new window whatever the
    # execution policy, which refuses .\script.ps1 on Windows 10 and 11
    $manager = Join-Path $script:RUSTDESK_PATH 'neodesk.ps1'
    foreach ($verb in '-Update', '-Uninstall', '-ResetPassword') {
        $want = "powershell -NoProfile -ExecutionPolicy Bypass -File `"$manager`" $verb"
        if (-not $out.Contains($want)) { Fail "the summary does not give $want" }
    }
    if ($out -match "& '") { Fail "the summary gives a command the execution policy refuses in a new window: $out" }
    # WD-2: it says how the server runs: the service under its own account
    if ($out -notmatch 'Runs as:\s+Windows service NeoDeskServer, as NT SERVICE\\NeoDeskServer') { Fail "the summary does not say the service runs as NT SERVICE\\NeoDeskServer: $out" }
    $text = Get-Content $installer -Raw
    foreach ($bare in "& '`$(Join-Path `$script:RUSTDESK_PATH 'neodesk.ps1')'", "& '`$manager'", "& '`$target'") {
        if ($text.Contains($bare)) { Fail "neodesk.ps1 still prints $bare" }
    }
    $script:FRESH_CONSOLE_ENV = $false
    Remove-Item -Recurse -Force $dir
}

# The installer's copy (neodesk.ps1 in the server's folder) comes from the
# release it installs, never from a branch's raw files: the folder this
# run downloaded from, else the release of the installed binary's version,
# or the "dev" release on the development channel. The release's
# SHA256SUMS checks it, and the installed binary the list's signature; a
# copy that fails, or a release without neodesk.ps1 (3.6.0-rc.4 and
# older), leaves the copy as it is.
function Test-InstallerComesFromTheRelease {
    $dir = New-TestInstall
    $rel = Join-Path $dir 'release'
    New-Item -ItemType Directory -Path $rel -Force | Out-Null
    $copy = Join-Path $script:RUSTDESK_PATH 'neodesk.ps1'
    $release = Join-Path $rel 'neodesk.ps1'
    Set-Content -Path $release -Value @('function Install-ManagerScript { }', "# the release's copy")
    $sum = (Get-FileHash -Path $release -Algorithm SHA256).Hash.ToLower()
    Set-Content -Path (Join-Path $rel 'SHA256SUMS') -Value "$sum  neodesk.ps1"
    Set-Content -Path (Join-Path $rel 'SHA256SUMS.sig') -Value 'c2lnbmF0dXJl'
    function Invoke-WebRequest {
        param($Uri, $OutFile, [switch]$UseBasicParsing, $ErrorAction)
        $script:fetched += "$Uri"
        $from = Join-Path $rel (($Uri -split '/')[-1])
        if (-not (Test-Path $from)) { throw [System.Net.WebException]::new('The remote server returned an error: (404) Not Found.') }
        Copy-Item -Path $from -Destination $OutFile -Force
    }
    function Get-ServerBinaryPath { 'fake-verify' }
    function Test-BinaryChecksFileLists { param($Path) $Path -eq 'fake-verify' }
    function fake-verify {
        if ($args[0] -eq '-version') { 'neodesk-server 3.6.0-rc.5 (built 2026-11-25T10:00:00Z, commit abc1234)'; return }
        $script:verifyArgs = @($args | ForEach-Object { "$_" })
        $global:LASTEXITCODE = if ($script:refuse) { 1 } else { 0 }
    }
    $script:SKIP_VERIFY = $false
    $script:refuse = $false
    $script:UPDATE_GITHUB_BRANCH_GIVEN = 'main'
    $script:UPDATE_GITHUB_BRANCH = 'main'
    $env:RELEASE_BASE_URL = $null
    # As the installed copy (an update)
    $script:SELF_PATH = $copy
    $installed = @('function Install-ManagerScript { }', '# the installed copy')

    foreach ($case in @(
            @{ Folder = 'https://example.test/releases/x'; Branch = 'main'; From = 'https://example.test/releases/x'; What = 'the folder this run installed from' },
            @{ Folder = ''; Branch = 'main'; From = 'https://github.com/neodeskapp/neodesk/releases/download/v3.6.0-rc.5'; What = "the installed binary's release" },
            @{ Folder = ''; Branch = 'dev'; From = 'https://github.com/neodeskapp/neodesk/releases/download/dev'; What = 'the development channel' })) {
        Set-Content -Path $copy -Value $installed
        $script:RELEASE_FOLDER_USED = $case.Folder
        $script:UPDATE_GITHUB_BRANCH = $case.Branch
        $script:UPDATE_GITHUB_BRANCH_GIVEN = $case.Branch
        $script:fetched = @()
        Install-ManagerScript 6> $null
        if ($script:fetched -notcontains "$($case.From)/neodesk.ps1") { Fail "$($case.What): neodesk.ps1 came from $($script:fetched -join ' ')" }
        if ((Get-FileHash $copy).Hash -ne (Get-FileHash $release).Hash) { Fail "$($case.What): the copy is not the release's neodesk.ps1" }
        if (($script:fetched -join ' ') -match 'githubusercontent') { Fail "$($case.What): a raw file was fetched" }
        if ($script:verifyArgs -notcontains '--sums') { Fail "$($case.What): the installed binary did not check the list's signature" }
    }
    $script:RELEASE_FOLDER_USED = 'https://example.test/releases/x'
    $script:UPDATE_GITHUB_BRANCH = 'main'
    $script:UPDATE_GITHUB_BRANCH_GIVEN = 'main'
    foreach ($case in @('signature', 'changed', 'none')) {
        Set-Content -Path $copy -Value $installed
        $before = (Get-FileHash $copy).Hash
        $script:refuse = $case -eq 'signature'
        if ($case -eq 'changed') { Add-Content -Path $release -Value '# changed' }
        if ($case -eq 'none') { Remove-Item -Path $release, (Join-Path $rel 'SHA256SUMS') -Force }
        Install-ManagerScript 6> $null
        if ((Get-FileHash $copy).Hash -ne $before) { Fail "a release copy that fails its check ($case) replaced the installer's copy" }
    }
    $script:refuse = $false
    $script:RELEASE_FOLDER_USED = ''
    Remove-Item -Recurse -Force $dir
}

# A250: the console's default port is 3375. An install whose .env names a
# port (CONSOLE_PORT or PORT) keeps it; one that names none moves to 3375
# at its update, which says so with how to keep 5000, and the firewall
# opens 3375.
function Test-UpdateMovesTheDefaultConsolePort {
    $dir = New-TestInstall
    $envFile = Join-Path $script:CONSOLE_PATH '.env'
    Set-Content -Path $envFile -Value @('SESSION_SECRET=kept')
    if (Test-ConsolePortNamed) { Fail 'an install without a port counts as one that names one' }
    if ((Get-ConsoleHttpPort) -ne '3375') { Fail "the default console port is $(Get-ConsoleHttpPort)" }
    $script:firewallRan = $false
    function Configure-Firewall { param([array]$MissingRules = @()) $script:firewallRan = $true }
    $out = (Show-ConsolePortMove -Named $false 6>&1 3>&1 | Out-String -Width 4096)
    if ($out -notmatch 'The console now answers on port 3375; it was 5000' -or $out -notmatch 'CONSOLE_PORT=5000') { Fail "the update does not say the console moved: $out" }
    if (-not $script:firewallRan) { Fail 'the update did not open 3375 in the firewall' }
    $script:firewallRan = $false
    $out = (Show-ConsolePortMove -Named $true 6>&1 3>&1 | Out-String -Width 4096)
    if ($out.Trim()) { Fail "an install that names its port was told it moved: $out" }
    if ($script:firewallRan) { Fail 'an install that names its port got firewall changes' }
    foreach ($setting in 'CONSOLE_PORT=5000', 'PORT=5080') {
        Set-Content -Path $envFile -Value @($setting)
        if (-not (Test-ConsolePortNamed)) { Fail "$setting does not count as a port the install names" }
    }
    if ((Get-ConsoleHttpPort) -ne '5080') { Fail "a configured port does not win: $(Get-ConsoleHttpPort)" }
    Remove-Item -Recurse -Force $dir
}

# -FromSource from a checkout: the script is in its install\ folder, and
# the source folders (neodesk-server, web-ui, remote-gateway) are the
# checkout's, one level up. A copy of the script elsewhere has none of
# them, and says so.
function Test-FromSourceFindsTheCheckout {
    $dir = Join-Path ([System.IO.Path]::GetTempPath()) ("nd-ps1-src-" + [guid]::NewGuid().ToString('N'))
    foreach ($part in 'install', 'neodesk-server', 'web-ui', 'remote-gateway') {
        New-Item -ItemType Directory -Path (Join-Path $dir $part) -Force | Out-Null
    }
    $alone = Join-Path $dir 'downloads'
    New-Item -ItemType Directory -Path $alone -Force | Out-Null
    # The installer's own top-level settings that build on its folder
    $settings = @($ast.EndBlock.Statements | Where-Object {
            $_ -is [System.Management.Automation.Language.AssignmentStatementAst] -and
            $_.Left.Extent.Text -eq '$script:GO_SERVER_SOURCE'
        })
    if (-not ($settings | Where-Object { $_.Left.Extent.Text -eq '$script:GO_SERVER_SOURCE' })) { Fail 'neodesk.ps1 sets no GO_SERVER_SOURCE' }
    $script:ScriptDir = Join-Path $dir 'install'
    foreach ($stmt in $settings) { . ([scriptblock]::Create($stmt.Extent.Text)) }
    if (-not (Test-Path (Join-Path $script:GO_SERVER_SOURCE 'go.mod') -PathType Leaf) -and -not (Test-Path $script:GO_SERVER_SOURCE -PathType Container)) {
        Fail "from a checkout's install folder the Go source is $($script:GO_SERVER_SOURCE), which is not there"
    }
    foreach ($part in 'web-ui', 'remote-gateway') {
        if (-not (Get-Command Get-RepoDir -ErrorAction SilentlyContinue) -or -not (Test-Path (Join-Path (Get-RepoDir) $part) -PathType Container)) { Fail "from a checkout's install folder $part is not found" }
    }
    # The functions take web-ui, remote-gateway and the old patch folder
    # from the checkout, not from the script's own folder
    $text = ($ast.FindAll({ $args[0] -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false) | ForEach-Object { $_.Extent.Text }) -join "`n"
    foreach ($part in 'web-ui', 'remote-gateway', 'hbbs-patch-v2') {
        if ($text -match [regex]::Escape('Join-Path $script:ScriptDir "' + $part)) { Fail "neodesk.ps1 looks for $part next to the script" }
    }
    if ($text -match [regex]::Escape('git -C $script:ScriptDir')) { Fail "neodesk.ps1 reads the commit of the script's folder" }
    # A copy alone: its own folder, without a source
    $script:ScriptDir = $alone
    foreach ($stmt in $settings) { . ([scriptblock]::Create($stmt.Extent.Text)) }
    if ($script:GO_SERVER_SOURCE -ne (Join-Path $alone 'neodesk-server')) { Fail "a copy alone looks for the source at $($script:GO_SERVER_SOURCE)" }
    if (Test-Path $script:GO_SERVER_SOURCE) { Fail 'a copy alone found a source' }
    Remove-Item -Recurse -Force $dir
}

# NeoDesk has no way from the old Node.js console: -RollbackGo and
# -CleanupNode are gone, and an install or an update on a server that
# still runs the Node.js console stops with what to do and changes nothing.
function Test-NoUpgradeFromTheNodeConsole {
    $params = @($ast.ParamBlock.Parameters | ForEach-Object { $_.Name.VariablePath.UserPath })
    foreach ($gone in 'RollbackGo', 'CleanupNode') {
        if ($params -contains $gone) { Fail "neodesk.ps1 still takes -$gone" }
    }
    $dir = New-TestInstall
    Set-Content -Path (Join-Path $script:CONSOLE_PATH 'server.js') -Value 'node'
    Set-Content -Path (Join-Path $script:CONSOLE_PATH '.env') -Value @('PORT=5000', 'SESSION_SECRET=kept')
    $before = (Get-FileHash (Join-Path $script:CONSOLE_PATH '.env')).Hash
    function Print-Header { }
    function Press-Enter { }
    function Detect-Installation { $script:INSTALL_STATUS = 'partial' }
    foreach ($action in 'Do-Update', 'Do-Install') {
        $out = (& $action 6>&1 | Out-String)
        if ($out -notmatch 'does not upgrade' -or $out -notmatch 'install NeoDesk on a clean server') { Fail "$action on an install with the Node.js console does not say what to do: $out" }
    }
    if ((Get-FileHash (Join-Path $script:CONSOLE_PATH '.env')).Hash -ne $before) { Fail 'a refused install changed .env' }
    if (Test-Path (Join-Path $script:CONSOLE_PATH '.env.pre-go')) { Fail 'a refused install started on the upgrade' }
    if ((Get-AutoAction) -ne 'install') { Fail "a run without questions on it does $(Get-AutoAction)" }
    Remove-Item -Recurse -Force $dir
}

# L431: neodesk, the command line, is on the Path after an install and an
# update: bin\neodesk.exe, a link to the server binary (the binary is the
# command line under that name), and the bin folder in the machine's Path
# once. A neodesk.exe of someone else stays; the uninstall takes the link
# and the folder out of the Path again.
function Test-CommandLineOnThePath {
    $dir = New-TestInstall
    $exe = Join-Path $script:RUSTDESK_PATH 'neodesk-server.exe'
    Set-Content -Path $exe -Value 'exe'
    $before = $script:machinePath
    Install-CommandLine 6> $null
    Install-CommandLine 6> $null
    $bin = Join-Path $script:RUSTDESK_PATH 'bin'
    $link = Get-Item -LiteralPath (Join-Path $bin 'neodesk.exe') -Force -ErrorAction SilentlyContinue
    if (-not $link -or $link.LinkType -ne 'SymbolicLink' -or "$($link.Target)" -ne $exe) {
        Fail "bin\neodesk.exe is not a link to the server binary: $($link.LinkType) $($link.Target)"
    }
    $found = @($script:machinePath -split ';' | Where-Object { $_ -eq $bin })
    if ($found.Count -ne 1) { Fail "the machine's Path has the command line's folder $($found.Count) times: $script:machinePath" }
    Set-Content -Path (Join-Path $script:RUSTDESK_PATH 'id_ed25519.pub') -Value 'KEY='
    $out = (Show-InstallSummary 6>&1 | Out-String)
    if (-not $out.Contains('neodesk --help')) { Fail "the summary does not name the command line: $out" }
    Remove-CommandLine 6> $null
    if ($script:machinePath -ne $before) { Fail "the uninstall left the Path as $script:machinePath" }
    if (Test-Path (Join-Path $bin 'neodesk.exe')) { Fail 'the uninstall left bin\neodesk.exe' }
    New-Item -ItemType Directory -Path $bin -Force | Out-Null
    Set-Content -Path (Join-Path $bin 'neodesk.exe') -Value 'mine'
    Install-CommandLine 6> $null 3> $null
    if ((Get-Content (Join-Path $bin 'neodesk.exe') -Raw).Trim() -ne 'mine') { Fail 'the install replaced a neodesk.exe of someone else' }
    $script:machinePath = $before
    Remove-Item -Recurse -Force $dir
}

# L155: the server's updater is told the repository of the releases, so an
# installed binary with an older default does not ask upstream; one named
# in .env stays.
function Test-UpdateAsksTheFork {
    $dir = New-TestInstall
    $script:UPDATE_GITHUB_OWNER, $script:UPDATE_GITHUB_REPO = 'neodeskapp', 'neodesk'
    $script:UPDATE_GITHUB_OWNER_GIVEN, $script:UPDATE_GITHUB_REPO_GIVEN = '', ''
    function script:Invoke-ServerCommand { param([string[]]$Arguments, $InputText, $Binary) $script:seenRepo = "$env:UPDATE_GITHUB_OWNER/$env:UPDATE_GITHUB_REPO"; return 1 }
    function script:Do-BackupSilent { }
    # The installed binary checks release signatures, so its updater runs
    function Test-BinaryVerifiesReleases { param($Path) $true }
    $script:BINARY_FILE = ''
    $script:FROM_SOURCE = $false
    $envFile = Join-Path $script:CONSOLE_PATH '.env'
    Set-Content -Path $envFile -Value @('SESSION_SECRET=kept')
    Update-GoInstall *> $null
    if ($script:seenRepo -ne 'neodeskapp/neodesk') { Fail "neodesk-server update ran for $($script:seenRepo)" }
    Set-Content -Path $envFile -Value @('UPDATE_GITHUB_OWNER=acme', 'UPDATE_GITHUB_REPO=desk')
    Update-GoInstall *> $null
    if ($script:seenRepo -ne 'acme/desk') { Fail "the repository of .env lost to the default: $($script:seenRepo)" }
    if ($env:UPDATE_GITHUB_OWNER) { Fail 'the update left UPDATE_GITHUB_OWNER in the environment' }
    Remove-Item -Recurse -Force $dir
}

# L67: both installers open the same ports: the API port of stock RustDesk
# clients without an API server setting (21114), RustDesk's (21115 to 21117
# TCP, 21116 UDP), the client API port, 21122 while CDAP is on, the relay
# nodes' control port (21123, off with NODE_CONTROL_PORT=0), and the
# console's ports unless TLS ends at a reverse proxy.
function Test-FirewallPorts {
    $dir = New-TestInstall
    $envFile = Join-Path $script:CONSOLE_PATH '.env'
    Set-Content -Path $envFile -Value @('API_PORT=21121', 'CONSOLE_PORT=5000', 'HTTPS_ENABLED=true', 'HTTPS_PORT=5443', 'CDAP_ENABLED=Y', 'TRUST_PROXY=false')
    $got = (Get-FirewallPorts | ForEach-Object { "$($_.Port)/$($_.Proto)" }) -join ' '
    if ($got -ne '21114/TCP 21115/TCP 21116/TCP 21116/UDP 21117/TCP 21121/TCP 21122/TCP 21123/TCP 5000/TCP 5443/TCP') { Fail "the firewall ports: $got" }
    Set-Content -Path $envFile -Value @('API_PORT=21121', 'CONSOLE_PORT=5000', 'HTTPS_ENABLED=false', 'CDAP_ENABLED=N', 'TRUST_PROXY=Y', 'NODE_CONTROL_PORT=0')
    $got = (Get-FirewallPorts | ForEach-Object { "$($_.Port)/$($_.Proto)" }) -join ' '
    if ($got -ne '21114/TCP 21115/TCP 21116/TCP 21116/UDP 21117/TCP 21121/TCP') { Fail "the firewall ports behind a reverse proxy without CDAP: $got" }
    Remove-Item -Recurse -Force $dir
}

# L66: the server binary makes the certificate, so Windows needs no
# OpenSSL; a certificate that could not be written as PEM files leaves the
# console on HTTP instead of turning HTTPS on without files.
function Test-CertificateFromTheBinary {
    $dir = New-TestInstall
    $envFile = Join-Path $script:CONSOLE_PATH '.env'
    Set-Content -Path $envFile -Value @('HTTPS_ENABLED=false', 'SSL_CERT_PATH=', 'SSL_KEY_PATH=', 'RUSTDESK_API_TLS=auto')
    function script:Get-PublicIP { '203.0.113.7' }
    function script:Get-LocalIP { '192.168.1.5' }
    function script:Test-BinaryHasCertCommand { $true }
    function script:Invoke-ServerCommand {
        param([string[]]$Arguments, $InputText, $Binary)
        $script:certArgs = $Arguments -join ' '
        $out = $Arguments[2]
        Set-Content -Path (Join-Path $out 'neodesk.crt') -Value 'crt'
        Set-Content -Path (Join-Path $out 'neodesk.key') -Value 'key'
        return 0
    }
    if (-not (Generate-SSLCertificates 6> $null)) { Fail 'the cert command made no certificate' }
    if ($script:certArgs -notmatch '--host 203\.0\.113\.7' -or $script:certArgs -notmatch '--host 192\.168\.1\.5') { Fail "the cert command was given: $($script:certArgs)" }
    if ((Get-ConsoleEnvValue -Key 'HTTPS_ENABLED') -ne 'true' -or (Get-ConsoleEnvValue -Key 'RUSTDESK_API_TLS') -ne 'false') { Fail "after the certificate .env has: $(Get-Content $envFile)" }

    Remove-Item -Recurse -Force (Join-Path $script:RUSTDESK_PATH 'ssl')
    Set-Content -Path $envFile -Value @('HTTPS_ENABLED=false', 'SSL_CERT_PATH=', 'SSL_KEY_PATH=')
    function script:Test-BinaryHasCertCommand { $false }
    function script:New-SelfSignedCertificate { [pscustomobject]@{ Thumbprint = 'AB' } }
    function script:Export-Certificate { }
    function script:Export-PfxCertificate { }
    function script:Get-Command { param($Name, $ErrorAction) if ($Name -eq 'openssl') { return $null }; Microsoft.PowerShell.Core\Get-Command $Name -ErrorAction SilentlyContinue }
    $ok = Generate-SSLCertificates 6> $null
    Remove-Item function:Get-Command, function:New-SelfSignedCertificate, function:Export-Certificate, function:Export-PfxCertificate -ErrorAction SilentlyContinue
    if ($ok -or (Get-ConsoleEnvValue -Key 'HTTPS_ENABLED') -eq 'true') { Fail "HTTPS was turned on without PEM files: $(Get-Content $envFile)" }
    Remove-Item -Recurse -Force $dir
}

# L161: passwords and secrets come from the system's cryptographic
# generator, with repeats allowed, never from Get-Random.
function Test-SecretsAreRandom {
    function script:Get-Random { throw 'Get-Random was used for a secret' }
    $a = Generate-RandomPassword -Length 64
    $b = Generate-RandomPassword -Length 64
    Remove-Item function:Get-Random -ErrorAction SilentlyContinue
    if ($a.Length -ne 64 -or $a -notmatch '^[A-Za-z0-9]+$') { Fail "a secret of 64 is '$a'" }
    if ($a -eq $b) { Fail 'two secrets are the same' }
    $repeats = @($a.ToCharArray() | Group-Object -CaseSensitive | Where-Object { $_.Count -gt 1 }).Count
    if ($repeats -eq 0) { Fail 'a secret of 64 has no character twice, which a shuffle of the alphabet gives' }
    if ((Get-Content $installer -Raw) -match 'Get-Random -Count') { Fail 'the installer still draws secrets with Get-Random -Count' }
}

# L70: for a binary that reads DATABASE_URL the installer names the
# database there alone; an older binary gets DB_TYPE and DB_URL as it reads
# them, and older .env files are read as the server reads them.
function Test-DatabaseUrlOnly {
    $dir = New-TestInstall
    $envFile = Join-Path $script:CONSOLE_PATH '.env'
    function Get-DatabaseLines { (@(Get-Content $envFile | Where-Object { $_ -match '^(DATABASE_URL|DB_URL|DB_TYPE|DB_PATH)=' }) -join ' ') }

    # The binary says by its warning text whether it reads DATABASE_URL
    $exe = Join-Path $dir 'probe.exe'
    Set-Content -Path $exe -Value 'upgrade-env --from FILE ... Remove DB_TYPE to use DATABASE_URL ...'
    if (-not (Test-BinaryReadsDatabaseUrl -Path $exe)) { Fail 'a binary with the DATABASE_URL warning is taken for an older one' }
    Set-Content -Path $exe -Value 'upgrade-env --from FILE ...'
    if (Test-BinaryReadsDatabaseUrl -Path $exe) { Fail 'an older binary is taken for one that reads DATABASE_URL' }

    function Get-ServerBinaryPath { 'fake-server' }
    function Test-BinaryHasSubcommands { param($Path) $true }
    function Test-BinaryReadsDatabaseUrl { param($Path) $true }
    function fake-server {
        if ($args[0] -eq 'env' -and $args[1] -eq 'merge') {
            $target = $args[3]
            if ($args -contains '--fresh') { Set-Content -Path $target -Value @() }
            foreach ($pair in @(@('DEFAULT_ADMIN_PASSWORD', $env:ND_SUBST_DEFAULT_ADMIN_PASSWORD), @('DATABASE_URL', $env:ND_SUBST_DATABASE_URL))) {
                if (-not (Get-Content $target | Where-Object { $_ -like "$($pair[0])=*" })) { Add-Content -Path $target -Value "$($pair[0])=$($pair[1])" }
            }
            Write-Output '{"success":true}'
        }
        $global:LASTEXITCODE = 0
    }
    $sqlite = Join-Path $script:RUSTDESK_PATH 'db_v2.sqlite3'
    $uri = 'postgres://bd:p&w|d@127.0.0.1:5432/bd?sslmode=disable'
    $env:ADMIN_PASSWORD = 'Database-Url-Pass-1'
    $script:USE_POSTGRESQL = $false
    $script:POSTGRESQL_URI = ''
    Merge-ConsoleEnv -FreshInstall $true *> $null
    if ((Get-DatabaseLines) -ne "DATABASE_URL=$sqlite") { Fail "a fresh SQLite install names its database as: $(Get-DatabaseLines)" }
    Remove-Item -Force $envFile
    $script:USE_POSTGRESQL = $true
    $script:POSTGRESQL_URI = $uri
    Merge-ConsoleEnv -FreshInstall $true *> $null
    if ((Get-DatabaseLines) -ne "DATABASE_URL=$uri") { Fail "a fresh PostgreSQL install names its database as: $(Get-DatabaseLines)" }
    $env:ADMIN_PASSWORD = $null

    # A move between databases leaves DATABASE_URL alone, also in an older file
    Set-Content -Path $envFile -Value @('SESSION_SECRET=kept', 'DB_TYPE=sqlite', 'DB_PATH=C:\old\db.sqlite3', 'DATABASE_URL=', 'DB_URL=C:\old\db.sqlite3')
    Use-DatabaseInEnv -Type postgres -Address $uri
    if ((Get-DatabaseLines) -ne "DATABASE_URL=$uri") { Fail "the move to PostgreSQL names the database as: $(Get-DatabaseLines)" }
    if ((Get-ConsoleEnvValue -Key 'SESSION_SECRET') -ne 'kept') { Fail 'the move lost another line of .env' }
    Use-SQLiteInstall 'C:\data\db_v2.sqlite3'
    if ((Get-DatabaseLines) -ne 'DATABASE_URL=C:\data\db_v2.sqlite3') { Fail "the move back to SQLite names the database as: $(Get-DatabaseLines)" }

    # An update keeps an older file as it is and keeps it on PostgreSQL; a
    # file of this installer names PostgreSQL by its address alone
    Set-Content -Path $envFile -Value @('DB_TYPE=postgres', "DATABASE_URL=$uri", "DB_URL=$uri")
    $script:USE_POSTGRESQL = $false
    $script:POSTGRESQL_URI = ''
    Preserve-DatabaseConfig *> $null
    if (-not $script:USE_POSTGRESQL -or $script:POSTGRESQL_URI -ne $uri) { Fail 'an older PostgreSQL install is not seen as one' }
    Merge-ConsoleEnv -FreshInstall $false *> $null
    if ((Get-DatabaseLines) -ne "DB_TYPE=postgres DATABASE_URL=$uri DB_URL=$uri") { Fail "an update changed an older file's database: $(Get-DatabaseLines)" }
    Set-Content -Path $envFile -Value @("DATABASE_URL=$uri")
    $script:USE_POSTGRESQL = $false
    Preserve-DatabaseConfig *> $null
    if (-not $script:USE_POSTGRESQL -or $script:POSTGRESQL_URI -ne $uri) { Fail 'DATABASE_URL alone is not seen as PostgreSQL' }

    # Read as the server reads them (envvar.DatabaseFrom)
    foreach ($case in @(
            @('postgres postgres://a@h/d', @('DB_TYPE=postgres', 'DATABASE_URL=postgres://a@h/d', 'DB_URL=postgres://a@h/d')),
            @('sqlite C:\old\db', @('DB_TYPE=sqlite', 'DATABASE_URL=', 'DB_URL=C:\old\db')),
            @('sqlite C:\old\db', @('DB_TYPE=sqlite', 'DATABASE_URL=postgres://a@h/d', 'DB_URL=C:\old\db')),
            @('sqlite ', @('DB_TYPE=SQLite', 'DATABASE_URL=postgres://a@h/d', 'DB_URL=postgresql://a@h/d')),
            @('postgres postgresql://a@h/d', @('DATABASE_URL=postgresql://a@h/d')),
            @('postgres postgres://a@h/d', @('DATABASE_URL=', 'DB_URL=postgres://a@h/d')),
            @('sqlite C:\srv\db.sqlite3', @('DATABASE_URL=sqlite://C:\srv\db.sqlite3', 'DB_URL=C:\other')),
            @('sqlite ', @('SESSION_SECRET=x')))) {
        Set-Content -Path $envFile -Value $case[1]
        $got = "$(Get-EnvDatabaseType) $(Get-EnvDatabaseUrl)"
        if ($got -ne $case[0]) { Fail "$($case[1] -join ' ') is read as '$got', want '$($case[0])'" }
    }
    $script:USE_POSTGRESQL = $false
    $script:POSTGRESQL_URI = ''
    Remove-Item -Recurse -Force $dir
}

# A152: a download is checked against the release's signed manifest by the
# installed neodesk-server (verify-release). A first install has no tool
# for the signature on Windows: it says plainly that the signature is
# checked from the second install on, and keeps the SHA-256 check. An
# update of a binary older than the release key goes through this download,
# not the old updater.
function Test-ReleaseSignatureOnWindows {
    $dir = New-TestInstall
    $asset = 'neodesk-server-windows-amd64.exe'
    $rel = Join-Path $dir 'release'
    $stage = Join-Path $dir 'stage'
    New-Item -ItemType Directory -Path $rel, $stage -Force | Out-Null
    Set-Content -Path (Join-Path $rel $asset) -Value 'the new server'
    $sum = (Get-FileHash -Path (Join-Path $rel $asset) -Algorithm SHA256).Hash.ToLower()
    Set-Content -Path (Join-Path $rel 'SHA256SUMS') -Value "$sum  $asset"
    Set-Content -Path (Join-Path $rel 'manifest.json') -Value @('{', '  "owner": "neodeskapp",', '  "branch": "main",', '  "files": []', '}')
    Set-Content -Path (Join-Path $rel 'manifest.sig') -Value 'c2lnbmF0dXJl'
    function Invoke-WebRequest {
        param($Uri, $OutFile, [switch]$UseBasicParsing, $ErrorAction)
        $name = ($Uri -split '/')[-1]
        $script:downloads += $name
        $from = Join-Path $rel $name
        if (-not (Test-Path $from)) { throw [System.Net.WebException]::new('The remote server returned an error: (404) Not Found.') }
        Copy-Item -Path $from -Destination $OutFile -Force
    }
    $script:UPDATE_GITHUB_BRANCH_GIVEN = ''
    $script:UPDATE_GITHUB_BRANCH = 'main'
    $script:SKIP_VERIFY = $false

    # The installed binary says by its help whether it checks signatures
    $probe = Join-Path $dir 'probe.exe'
    Set-Content -Path $probe -Value 'upgrade-env --from FILE ... verify-release --manifest FILE --signature FILE ...'
    if (-not (Test-BinaryVerifiesReleases -Path $probe)) { Fail 'a binary with verify-release is taken for an older one' }
    Set-Content -Path $probe -Value 'upgrade-env --from FILE ...'
    if (Test-BinaryVerifiesReleases -Path $probe) { Fail 'an older binary is taken for one that checks signatures' }

    # A first install: no installed binary
    $script:downloads = @()
    $out = (Get-ReleaseBinary -Dir $stage 6>&1 | Out-String -Width 4096)
    if (-not (Test-Path (Join-Path $stage 'neodesk-server.exe'))) { Fail "a first install with a matching SHA-256 installed nothing: $out" }
    if ($out -notmatch 'checked from the second install on') { Fail "a first install does not say when the signature is checked: $out" }
    if ($script:downloads -contains 'manifest.sig') { Fail 'a first install fetched a signature it cannot check' }

    # An installed binary with verify-release checks the download
    function Get-ServerBinaryPath { 'fake-verify' }
    function Test-BinaryVerifiesReleases { param($Path) $Path -eq 'fake-verify' }
    function fake-verify {
        $script:verifyArgs = @($args | ForEach-Object { "$_" })
        if ($script:refuse) { Write-Output 'neodesk-server verify-release: the signature of the manifest does not verify against the release key'; $global:LASTEXITCODE = 1; return }
        Write-Output "verified: neodesk-server 3.6.0 (0123456), $($args[6])"
        $global:LASTEXITCODE = 0
    }
    $script:refuse = $false
    Remove-Item -Recurse -Force $stage; New-Item -ItemType Directory -Path $stage | Out-Null
    $out = (Get-ReleaseBinary -Dir $stage 6>&1 | Out-String -Width 4096)
    if (-not (Test-Path (Join-Path $stage 'neodesk-server.exe'))) { Fail "a release the installed binary checked was not installed: $out" }
    $want = "verify-release --manifest $(Join-Path $stage 'manifest.json') --signature $(Join-Path $stage 'manifest.sig') --name $asset $(Join-Path $stage $asset)"
    if (($script:verifyArgs -join ' ') -ne $want) { Fail "verify-release ran as: $($script:verifyArgs -join ' ')" }
    if ($out -notmatch 'checked by the installed neodesk-server') { Fail "the download does not say who checked the signature: $out" }
    $script:refuse = $true
    Remove-Item -Recurse -Force $stage; New-Item -ItemType Directory -Path $stage | Out-Null
    $out = (Get-ReleaseBinary -Dir $stage 6>&1 | Out-String -Width 4096)
    if (Test-Path (Join-Path $stage 'neodesk-server.exe')) { Fail 'a release the installed binary refused was installed' }
    if ($out -notmatch 'does not verify' -or $out -notmatch '-SkipVerify') { Fail "the refusal does not say why and what to do: $out" }
    $script:refuse = $false
    $script:UPDATE_GITHUB_BRANCH = 'dev'
    Remove-Item -Recurse -Force $stage; New-Item -ItemType Directory -Path $stage | Out-Null
    $out = (Get-ReleaseBinary -Dir $stage 6>&1 | Out-String -Width 4096)
    if (Test-Path (Join-Path $stage 'neodesk-server.exe')) { Fail "a stable release was installed for the development channel: $out" }
    $script:UPDATE_GITHUB_BRANCH = 'main'
    Remove-Item -Force (Join-Path $rel 'manifest.sig')
    Remove-Item -Recurse -Force $stage; New-Item -ItemType Directory -Path $stage | Out-Null
    $out = (Get-ReleaseBinary -Dir $stage 6>&1 | Out-String -Width 4096)
    if (Test-Path (Join-Path $stage 'neodesk-server.exe')) { Fail 'a release without a signature was installed' }
    if ($out -notmatch 'no signed manifest') { Fail "a release without a signature says: $out" }

    # An update of an older binary does not run its updater
    function Test-BinaryVerifiesReleases { param($Path) $false }
    function Do-BackupSilent { }
    function Invoke-ServerCommand { param([string[]]$Arguments, $InputText, $Binary) $script:updaterRan = $true; return 1 }
    function Get-StagedServerBinary { $script:ownDownload = $true; $script:STAGED_SERVER_DIR = Join-Path $dir 'staged'; return $false }
    $script:updaterRan = $false
    $script:ownDownload = $false
    $script:BINARY_FILE = ''
    $script:FROM_SOURCE = $false
    Update-GoInstall *> $null
    if ($script:updaterRan) { Fail "the older binary's updater ran" }
    if (-not $script:ownDownload) { Fail "the update of an older binary did not download and check the release itself" }
    Remove-Item -Recurse -Force $dir
}

# L210 (A183): while the repository has no stable release, the stable
# channel installs the newest pre-release that is not the rolling dev one,
# also with -Channel stable, and says which release it took; the
# development build only when there is no pre-release either.
function Test-NoStableReleaseTakesTheNewestPrerelease {
    $dir = New-TestInstall
    $asset = 'neodesk-server-windows-amd64.exe'
    $rel = Join-Path $dir 'rc2'
    New-Item -ItemType Directory -Path $rel -Force | Out-Null
    Set-Content -Path (Join-Path $rel $asset) -Value 'candidate'
    $sum = (Get-FileHash -Path (Join-Path $rel $asset) -Algorithm SHA256).Hash.ToLower()
    Set-Content -Path (Join-Path $rel 'SHA256SUMS') -Value "$sum  $asset"
    $script:releases = @(
        [pscustomobject]@{ tag_name = 'dev'; draft = $false; prerelease = $true },
        [pscustomobject]@{ tag_name = 'v3.7.0-rc.1'; draft = $true; prerelease = $true },
        [pscustomobject]@{ tag_name = 'v3.6.0-rc.2'; draft = $false; prerelease = $true },
        [pscustomobject]@{ tag_name = 'v3.6.0-rc.1'; draft = $false; prerelease = $true })
    # L254: GitHub's API says there is no stable release (404), and the
    # download from releases/latest fails the way Windows PowerShell 5.1
    # reports it on this route: a closed connection without a status, which
    # the install took for a network failure
    $script:latest = $null
    function Invoke-RestMethod {
        param($Uri, $Headers, [switch]$UseBasicParsing, $ErrorAction)
        if ($Uri -eq 'https://api.github.com/repos/neodeskapp/neodesk/releases/latest') {
            if ($script:latest) { return $script:latest }
            throw [System.Net.WebException]::new('The remote server returned an error: (404) Not Found.')
        }
        if ($Uri -ne 'https://api.github.com/repos/neodeskapp/neodesk/releases?per_page=50') { throw "unexpected $Uri" }
        return $script:releases
    }
    function Invoke-WebRequest {
        param($Uri, $OutFile, [switch]$UseBasicParsing, $ErrorAction)
        if ($Uri -like 'https://github.com/neodeskapp/neodesk/releases/latest/*') { throw [System.Net.WebException]::new('The underlying connection was closed: The connection was closed unexpectedly.') }
        if ($Uri -notlike 'https://github.com/neodeskapp/neodesk/releases/download/v3.6.0-rc.2/*' -and $Uri -notlike 'https://github.com/neodeskapp/neodesk/releases/download/v3.6.0/*') { throw [System.Net.WebException]::new('The remote server returned an error: (404) Not Found.') }
        Copy-Item -Path (Join-Path $rel (($Uri -split '/')[-1])) -Destination $OutFile -Force
    }
    $script:UPDATE_GITHUB_OWNER, $script:UPDATE_GITHUB_REPO = 'neodeskapp', 'neodesk'
    $script:SKIP_VERIFY = $true
    foreach ($given in @('', 'main')) {
        $stage = Join-Path $dir "stage-$given"
        New-Item -ItemType Directory -Path $stage -Force | Out-Null
        $script:UPDATE_GITHUB_BRANCH_GIVEN = $given
        $script:UPDATE_GITHUB_BRANCH = 'main'
        $out = (Get-ReleaseBinary -Dir $stage 6>&1 | Out-String -Width 4096)
        if (-not (Test-Path (Join-Path $stage 'neodesk-server.exe'))) { Fail "the newest pre-release was not installed (given '$given'): $out" }
        if ($out -notmatch 'no stable release yet: installing the newest pre-release v3\.6\.0-rc\.2') { Fail "the install does not say which release it took: $out" }
        if ($script:UPDATE_GITHUB_BRANCH -ne 'main') { Fail "the install left the stable channel for $($script:UPDATE_GITHUB_BRANCH)" }
    }
    # Without a pre-release: the development build
    $script:releases = @([pscustomobject]@{ tag_name = 'dev'; draft = $false; prerelease = $true })
    $script:UPDATE_GITHUB_BRANCH_GIVEN = ''
    $script:UPDATE_GITHUB_BRANCH = 'main'
    $out = (Get-ReleaseBinary -Dir (Join-Path $dir 'stage-') 6>&1 | Out-String -Width 4096)
    if ($out -notmatch 'no stable release or pre-release yet: installing the development build') { Fail "without a pre-release the install does not take the development build: $out" }
    # With the stable channel given there is no development build to fall
    # back to: the install says the repository has no release
    $script:UPDATE_GITHUB_BRANCH_GIVEN = 'main'
    $script:UPDATE_GITHUB_BRANCH = 'main'
    $out = (Get-ReleaseBinary -Dir (Join-Path $dir 'stage-none') 6>&1 | Out-String -Width 4096)
    if ($out -notmatch 'has no stable release or pre-release yet' -or $out -match 'Could not reach') { Fail "a repository without releases is reported as: $out" }
    # A stable release: its files by its tag
    $script:latest = [pscustomobject]@{ tag_name = 'v3.6.0'; draft = $false; prerelease = $false }
    $stage = Join-Path $dir 'stage-stable'
    New-Item -ItemType Directory -Path $stage -Force | Out-Null
    $out = (Get-ReleaseBinary -Dir $stage 6>&1 | Out-String -Width 4096)
    if (-not (Test-Path (Join-Path $stage 'neodesk-server.exe')) -or $out -notmatch 'releases/download/v3\.6\.0') { Fail "the stable release was not installed by its tag: $out" }
    $script:latest = $null
    $script:UPDATE_GITHUB_BRANCH_GIVEN = ''
    $script:UPDATE_GITHUB_BRANCH = 'main'
    $env:UPDATE_GITHUB_BRANCH = $null
    $script:SKIP_VERIFY = $false
    Remove-Item -Recurse -Force $dir
}

# L71: the installed binary's migrate command moves the database; the
# separate tool of an older release is the fallback.
function Test-MigrateCommand {
    $dir = New-TestInstall
    $probe = Join-Path $dir 'probe.exe'
    Set-Content -Path $probe -Value 'upgrade-env --from FILE ... migrate -mode sqlite2pg|pg2sqlite|rust2go ...'
    if (-not (Test-BinaryMigrates -Path $probe)) { Fail 'a binary with the migrate command is taken for an older one' }
    Set-Content -Path $probe -Value 'upgrade-env --from FILE ...'
    if (Test-BinaryMigrates -Path $probe) { Fail 'an older binary is taken for one with the migrate command' }

    function Get-ServerBinaryPath { 'fake-server' }
    function Test-BinaryMigrates { param($Path) $true }
    function fake-server { $script:serverCalls += ($args -join ' '); $global:LASTEXITCODE = 0 }
    $script:serverCalls = @()
    $src = Join-Path $script:RUSTDESK_PATH 'db_v2.sqlite3'
    Set-Content -Path $src -Value 'db'
    Set-Content -Path (Join-Path $script:CONSOLE_PATH '.env') -Value @('SESSION_SECRET=kept')
    $uri = 'postgres://u:p@h/db?sslmode=disable'
    $script:USE_POSTGRESQL = $true
    $script:POSTGRESQL_URI = $uri
    $script:AUTO_MODE = $true
    if ((Get-MigrateBin) -ne 'Invoke-ServerMigrate') { Fail "the migrate tool is $(Get-MigrateBin), not the server's command" }
    Migrate-SQLiteToPostgreSQL *> $null
    if ($script:serverCalls -notcontains "migrate -mode sqlite2pg -src $src -dst $uri") { Fail "the switch ran the server as: $($script:serverCalls -join '; ')" }
    if ($script:serverCalls -notcontains "migrate -mode backup -src $src") { Fail "the switch made no backup with the migrate command: $($script:serverCalls -join '; ')" }
    if ((Get-ConsoleEnvValue -Key 'DATABASE_URL') -ne $uri) { Fail "after the move .env does not point at PostgreSQL: $(Get-Content (Join-Path $script:CONSOLE_PATH '.env'))" }
    $script:USE_POSTGRESQL = $false
    $script:POSTGRESQL_URI = ''
    Remove-Item -Recurse -Force $dir
}

# L73: the installer's backup is the server's own (database, PostgreSQL
# too, settings, the console's data) plus the keys; without the backup
# command, or when it fails, the SQLite file is copied as before.
function Test-BackupUsesTheServersBackup {
    $dir = New-TestInstall
    $probe = Join-Path $dir 'probe.exe'
    Set-Content -Path $probe -Value 'upgrade-env --from FILE ... backup [--out DIR] [--db DSN] ...'
    if (-not (Test-BinaryBacksUp -Path $probe)) { Fail 'a binary with the backup command is taken for an older one' }
    Set-Content -Path $probe -Value 'upgrade-env --from FILE ...'
    if (Test-BinaryBacksUp -Path $probe) { Fail 'an older binary is taken for one with the backup command' }

    # The installer's own Do-BackupSilent (an earlier check stands one in)
    $real = $ast.FindAll({ $args[0] -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $args[0].Name -eq 'Do-BackupSilent' }, $false) | Select-Object -First 1
    . ([scriptblock]::Create($real.Extent.Text))
    $script:BACKUP_DIR = Join-Path $dir 'backups'
    $script:DB_PATH = Join-Path $script:RUSTDESK_PATH 'db_v2.sqlite3'
    Set-Content -Path $script:DB_PATH -Value 'sqlite file'
    Set-Content -Path (Join-Path $script:RUSTDESK_PATH 'id_ed25519') -Value 'private key'
    function Get-ZipNames([string]$Path) {
        Add-Type -AssemblyName System.IO.Compression.FileSystem
        $z = [System.IO.Compression.ZipFile]::OpenRead($Path)
        try { return @($z.Entries | ForEach-Object { $_.Name }) } finally { $z.Dispose() }
    }
    function Test-BinaryBacksUp { param($Path) $true }
    function Invoke-ServerCommand {
        param([string[]]$Arguments, $InputText, $Binary)
        $script:backupArgs = $Arguments
        if ($script:backupFails) { return 1 }
        Set-Content -Path (Join-Path $Arguments[2] 'neodesk-full-backup.zip') -Value 'archive'
        return 0
    }
    $script:backupFails = $false
    Do-BackupSilent *> $null
    $zip = @(Get-ChildItem -Path $script:BACKUP_DIR -Filter '*.zip')[0]
    if (-not $zip) { Fail 'no backup was made' }
    else {
        if ($script:backupArgs[0] -ne 'backup' -or $script:backupArgs[1] -ne '--out' -or $script:backupArgs[2] -notlike (Join-Path $script:BACKUP_DIR 'neodesk_backup_*')) { Fail "the server's backup ran as: $($script:backupArgs -join ' ')" }
        $names = Get-ZipNames $zip.FullName
        if ($names -notcontains 'neodesk-full-backup.zip' -or $names -notcontains 'id_ed25519') { Fail "the backup holds: $($names -join ', ')" }
        if ($names -contains 'db_v2.sqlite3') { Fail 'the backup copied the live SQLite file beside the server backup' }
        Remove-Item -Force $zip.FullName
    }
    $script:backupFails = $true
    $out = (Do-BackupSilent 6>&1 | Out-String -Width 4096)
    if ($out -notmatch 'neodesk-server backup failed') { Fail "a failed server backup is not said: $out" }
    $zip = @(Get-ChildItem -Path $script:BACKUP_DIR -Filter '*.zip')[0]
    $names = Get-ZipNames $zip.FullName
    if ($names -notcontains 'db_v2.sqlite3') { Fail "without the server backup the database file is not in the backup: $($names -join ', ')" }
    Remove-Item -Recurse -Force $dir
}

# Get-HostLines BLOCK: what BLOCK wrote with Write-Host, as lines, and the
# console colours it set that are not the host's own
function Get-HostLines([scriptblock]$Block) {
    $text = New-Object System.Text.StringBuilder
    $colors = @()
    foreach ($record in @(& $Block 6>&1)) {
        $m = $record.MessageData
        if ($m -is [System.Management.Automation.HostInformationMessage]) {
            [void]$text.Append($m.Message)
            if (-not $m.NoNewLine) { [void]$text.Append("`n") }
            if ($m.ForegroundColor -ne $Host.UI.RawUI.ForegroundColor) { $colors += [string]$m.ForegroundColor }
            if ($m.BackgroundColor -ne $Host.UI.RawUI.BackgroundColor) { $colors += [string]$m.BackgroundColor }
        } else {
            [void]$text.Append("$m`n")
        }
    }
    [pscustomobject]@{ Lines = @($text.ToString().TrimEnd("`r", "`n") -split "`r?`n"); Colors = $colors }
}

# Get-Contrast R G B: the lowest contrast of the colour on black, on
# PowerShell's blue (#012456) and on white
function Get-Contrast([int]$R, [int]$G, [int]$B) {
    $lum = {
        param([int[]]$rgb)
        $c = foreach ($v in $rgb) { $v = $v / 255; if ($v -le 0.04045) { $v / 12.92 } else { [math]::Pow(($v + 0.055) / 1.055, 2.4) } }
        0.2126 * $c[0] + 0.7152 * $c[1] + 0.0722 * $c[2]
    }
    $l = & $lum @($R, $G, $B)
    $min = 99
    foreach ($bg in @(@(12, 12, 12), @(1, 36, 86), @(255, 255, 255))) {
        $lb = & $lum $bg
        $ratio = ([math]::Max($l, $lb) + 0.05) / ([math]::Min($l, $lb) + 0.05)
        if ($ratio -lt $min) { $min = $ratio }
    }
    return $min
}

# The header: in a console of 120 columns the NeoDesk logo in 15 lines of
# half blocks, with the title on its 13th, beside the mark under the
# wordmark; "NeoDesk <title>" on one line in a console narrower than the
# logo; one plain line when the output goes to a file or a pipe or
# NO_COLOR is set, and no escape codes then. No box characters, no line
# wider than 119 columns. The status helpers use no blue or magenta, and
# their 24-bit colours keep 3.5:1 on black, PowerShell's blue and white.
function Test-HeaderAndColours {
    function Clear-Host { }
    function Get-ConsoleWidth { $script:consoleWidth }
    $dir = New-TestInstall
    $script:VERSION = [regex]::Match((Get-Content $installer -Raw), '(?m)^\$script:VERSION = "([^"]+)"').Groups[1].Value
    $title = "Server installer $($script:VERSION)"
    $plain = "NeoDesk $title"
    $esc = [string][char]27
    $banned = 'Blue', 'DarkBlue', 'Magenta', 'DarkMagenta'
    # Box drawing (U+2500 to U+257F) and the half and full blocks of the mark
    $boxChars = '[' + [char]0x2500 + '-' + [char]0x257F + ']'
    $halfBlocks = '[' + [char]0x2580 + [char]0x2584 + [char]0x2588 + ']'
    $helpers = { Print-Success 's'; Print-Error 'e'; Print-Warning 'w'; Print-Info 'i'; Print-Step 'p' }
    $savedNoColor = $env:NO_COLOR

    # A file or a pipe: a run of its own with its output redirected
    $child = Join-Path $dir 'header.ps1'
    Set-Content -Path $child -Encoding ASCII -Value @(
        '$tokens = $null; $errors = $null'
        "`$ast = [System.Management.Automation.Language.Parser]::ParseFile('$installer', [ref]`$tokens, [ref]`$errors)"
        'foreach ($fn in $ast.FindAll({ $args[0] -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) { . ([scriptblock]::Create($fn.Extent.Text)) }'
        "`$script:VERSION = '$($script:VERSION)'; `$script:LOG_FILE = '$(Join-Path $dir 'child.log')'"
        'Print-Header; Print-Info "a note"'
    )
    $env:NO_COLOR = $null
    $out = @(& (Get-Process -Id $PID).Path -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $child)
    if (($out -join "`n").Contains($esc)) { Fail "the header has escape codes in a pipe: $($out -join ' | ')" }
    if ($out[0] -ne $plain) { Fail "in a pipe the header is not one plain line: $($out -join ' | ')" }

    # A console, with and without NO_COLOR
    function Test-OutputRedirected { $false }
    $script:COLOR_MODE = $null
    if ((Get-ColorMode) -eq 'none') { Fail 'a console without NO_COLOR gets no colour' }
    $env:NO_COLOR = '1'
    $script:COLOR_MODE = $null
    $got = Get-HostLines { Print-Header; & $helpers }
    if (($got.Lines -join "`n").Contains($esc)) { Fail "with NO_COLOR there are escape codes: $($got.Lines -join ' | ')" }
    if ($got.Lines[0] -ne $plain) { Fail "with NO_COLOR the header is not one plain line: $($got.Lines -join ' | ')" }
    if ($got.Colors) { Fail "with NO_COLOR there are console colours: $($got.Colors -join ', ')" }
    $env:NO_COLOR = $savedNoColor

    foreach ($mode in 'vt', 'console') {
        $script:COLOR_MODE = $mode
        $script:consoleWidth = 120
        $got = Get-HostLines { Print-Header }
        $text = @($got.Lines | ForEach-Object { $_ -replace "$esc\[[0-9;]*m", '' })
        if (($text -join "`n") -match $boxChars) { Fail "the $mode header draws a box: $($text -join ' | ')" }
        $logoLines = @($text | Where-Object { $_ -match $halfBlocks })
        if ($logoLines.Count -ne 15) {
            Fail "the $mode header has $($logoLines.Count) lines of the logo, not 15"
        } elseif (($logoLines[12] -replace "^$halfBlocks* *", '') -ne $title) {
            Fail "the $mode header does not have $title beside the mark under the wordmark: $($text -join ' | ')"
        }
        foreach ($line in $text) {
            if ($line.Length -gt 119) { Fail "the $mode header has a line of $($line.Length) columns: $line" }
            if ($line -match '\s$') { Fail "the $mode header has a line that ends in spaces: '$line'" }
        }
        if ($text -notcontains '[!] Windows support is experimental.') { Fail "the $mode header does not say that Windows support is experimental" }
        if ($mode -eq 'vt' -and ($got.Lines -join '') -notmatch '38;2;88;166;255') { Fail 'the vt header does not draw the mark in its 24-bit colours' }
        foreach ($c in $got.Colors) { if ($banned -contains $c) { Fail "the $mode header uses $c" } }

        $script:consoleWidth = 118
        $got = Get-HostLines { Print-Header }
        $text = @($got.Lines | ForEach-Object { $_ -replace "$esc\[[0-9;]*m", '' } | Where-Object { $_ })
        if ($text[0] -ne $plain -or ($text -join "`n") -match $halfBlocks) { Fail "in a console of 118 columns the $mode header is not one line: $($text -join ' | ')" }

        $got = Get-HostLines $helpers
        foreach ($c in $got.Colors) { if ($banned -contains $c) { Fail "a status helper uses $c" } }
        if ($mode -eq 'vt') {
            $rgb = @($got.Lines | ForEach-Object { [regex]::Matches($_, '38;2;(\d+);(\d+);(\d+)') })
            if ($rgb.Count -lt 4) { Fail "the status helpers have no 24-bit colours: $($got.Lines -join ' | ')" }
            foreach ($m in $rgb) {
                $min = Get-Contrast ([int]$m.Groups[1].Value) ([int]$m.Groups[2].Value) ([int]$m.Groups[3].Value)
                if ($min -lt 3.5) { Fail ("the status colour {0} has {1:N2}:1 on black, blue or white" -f $m.Value, $min) }
            }
        }
    }
    $script:COLOR_MODE = $null
    Remove-Item -Recurse -Force $dir
}

# The menus' title box and descriptions use no colour that is hard to read
# on PowerShell's blue: no blue, magenta or dark gray.
function Test-MenuColours {
    function Clear-Host { }
    function Test-TuiAvailable { $false }
    function Read-Host { param($Prompt) '1' }
    $got = Get-HostLines { Invoke-MenuChoose -Title 'Title' -Subtitle 'Subtitle' -Items @("One`tthe first", "Back`tthe menu before") -Returns @('1', '0') }
    if (-not $got.Colors) { Fail 'the menu has no colours to check' }
    foreach ($c in $got.Colors) {
        if ('Blue', 'DarkBlue', 'Magenta', 'DarkMagenta', 'DarkGray' -contains $c) { Fail "the menu uses $c" }
    }
}

# The installer's own Get-ServerBinaryPath again, which earlier tests
# replaced with stand-ins
function Use-RealServerBinaryPath {
    foreach ($fn in $ast.FindAll({ $args[0] -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $args[0].Name -eq 'Get-ServerBinaryPath' }, $false)) {
        . ([scriptblock]::Create(($fn.Extent.Text -replace '^function ', 'function script:')))
    }
}

# The command ASTs named NAME inside the installer's function FUNCTION
function Get-CommandsIn([string]$Function, [string]$Name) {
    $fn = $ast.FindAll({ $args[0] -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $args[0].Name -eq $Function }, $false) | Select-Object -First 1
    if (-not $fn) { return @() }
    return @($fn.Body.FindAll({ $args[0] -is [System.Management.Automation.Language.CommandAst] -and $args[0].GetCommandName() -eq $Name }, $true))
}

# WD-1: the install's folders at the drive root hold the keys, .env and
# the database. They lose the ACL they inherit from C:\ (every user reads
# it on Windows Server, changes it on Windows 10 and 11): Administrators
# and SYSTEM by SID, the service's account while the service exists. The
# install locks them before the keys are made, the update too, and the
# repair never turns inheritance back on.
function Test-FoldersAreLocked {
    $dir = New-TestInstall
    New-Item -ItemType Directory -Path $script:BACKUP_DIR -Force | Out-Null
    Protect-InstallFolders 3> $null | Out-Null
    foreach ($path in $script:RUSTDESK_PATH, $script:CONSOLE_PATH, $script:BACKUP_DIR) {
        $call = $script:icaclsCalls | Where-Object { $_[0] -eq $path -and $_ -contains '/inheritance:r' } | Select-Object -First 1
        if (-not $call) { Fail "$path keeps the ACL it inherits: $(($script:icaclsCalls | ForEach-Object { $_ -join ' ' }) -join ' | ')"; continue }
        foreach ($grant in '/grant:r', '*S-1-5-32-544:(OI)(CI)F', '*S-1-5-18:(OI)(CI)F') {
            if ($call -notcontains $grant) { Fail "the lock of $path lacks $grant`: $($call -join ' ')" }
        }
    }
    if ((($script:icaclsCalls | ForEach-Object { $_ -join ' ' }) -join "`n") -match 'NT SERVICE') { Fail 'without the service the folders name its account' }
    function Get-Service { param($Name, $ErrorAction) [pscustomobject]@{ Name = $Name } }
    $script:icaclsCalls = @()
    Protect-InstallFolders 3> $null | Out-Null
    $granted = @($script:icaclsCalls | Where-Object { ($_ -join ' ') -match 'NT SERVICE\\NeoDeskServer:\(OI\)\(CI\)M' } | ForEach-Object { $_[0] })
    if (($granted -join '|') -ne "$($script:RUSTDESK_PATH)|$($script:CONSOLE_PATH)") { Fail "the service's account may change: $($granted -join ', ')" }
    Remove-Item function:Get-Service -ErrorAction SilentlyContinue

    function Invoke-ManagementCli { param($Action, $Arguments) $true }
    Set-Content -Path (Join-Path $script:CONSOLE_PATH '.env') -Value 'SESSION_SECRET=kept'
    Set-Content -Path (Join-Path $script:RUSTDESK_PATH 'id_ed25519') -Value 'key'
    $script:icaclsCalls = @()
    Repair-ManagementPermissions *> $null
    $all = @($script:icaclsCalls | ForEach-Object { $_ -join ' ' })
    if (($all -join "`n") -match '/inheritance:e') { Fail "the repair turns inheritance back on: $($all -join ' | ')" }
    if (($all -join "`n") -match '(^| )(Administrators|SYSTEM):') { Fail "the repair names accounts in English, which fails on other languages: $($all -join ' | ')" }
    foreach ($secret in (Join-Path $script:CONSOLE_PATH '.env'), (Join-Path $script:RUSTDESK_PATH 'id_ed25519')) {
        if (-not ($script:icaclsCalls | Where-Object { $_[0] -eq $secret -and $_ -contains '/inheritance:r' })) { Fail "the repair does not lock $secret" }
    }

    if ((Get-CommandsIn 'Update-GoInstall' 'Protect-InstallFolders').Count -eq 0) { Fail 'an update does not lock the folders' }
    $lock = @(Get-CommandsIn 'Do-Install' 'Protect-InstallFolders')
    $certs = @(Get-CommandsIn 'Do-Install' 'Generate-SSLCertificates')
    $services = @(Get-CommandsIn 'Do-Install' 'Setup-Services')
    if ($lock.Count -eq 0) { Fail 'an install does not lock the folders' }
    elseif ($lock[0].Extent.StartOffset -gt $certs[0].Extent.StartOffset -or $lock[0].Extent.StartOffset -gt $services[0].Extent.StartOffset) { Fail 'the install locks the folders after it made the keys' }
    Remove-Item -Recurse -Force $dir
}



# WD-3: the admin's first password is in the summary, and the summary is
# the last thing of the install: no menu, no question and no cleared
# screen after it, only Enter once the admin copied it. The folders are
# locked before the certificates are made.
function Test-PasswordIsTheLastThingShown {
    $dir = New-TestInstall
    $script:AUTO_MODE = $false
    $script:MINIMAL_MODE = $false
    $env:ADMIN_PASSWORD = 'Shown-Once-Pass-1'
    # What the install does, in order: its text, and EVENT lines from the
    # stand-ins
    function Print-Header { Write-Host 'EVENT clear' }
    function Clear-Host { Write-Host 'EVENT clear' }
    function Detect-Installation { $script:INSTALL_STATUS = 'none' }
    function Test-NodeEraInstall { $false }
    function Choose-DatabaseType { }
    function Install-Binaries { $true }
    function Setup-ConsoleHome { $script:FRESH_CONSOLE_ENV = $true; $true }
    function Generate-SSLCertificates { $true }
    function Setup-Services { $true }
    function Configure-Firewall { $true }
    function Start-ServicesWithVerification { $true }
    function Remove-FirstAdminPassword { }
    function Install-ManagerScript { }
    function Install-CommandLine { }
    function Get-PublicIP { '203.0.113.7' }
    function Get-LocalIP { '127.0.0.1' }
    function Do-ConfigureSSL { Write-Host 'EVENT ssl menu' }
    function Confirm-Action { param($Prompt) Write-Host "EVENT ask: $Prompt"; $true }
    function Press-Enter { Write-Host 'EVENT enter' }
    function Read-Host { param($Prompt) Write-Host "EVENT enter: $Prompt"; '' }
    $list = @(Do-Install 6>&1 | Where-Object { $_ -is [System.Management.Automation.InformationRecord] } | ForEach-Object { "$_" })
    $env:ADMIN_PASSWORD = $null
    $script:AUTO_MODE = $true
    $shown = @(0..($list.Count - 1) | Where-Object { $list[$_] -like '*Shown-Once-Pass-1*' })
    if ($shown.Count -eq 0) { Fail 'the install does not show the admin password'; Remove-Item -Recurse -Force $dir; return }
    $after = @($list[($shown[-1] + 1)..($list.Count - 1)])
    $bad = @($after | Where-Object { $_ -eq 'EVENT clear' -or $_ -eq 'EVENT ssl menu' -or $_ -like 'EVENT ask: *' })
    if ($bad.Count -gt 0) { Fail "after the password the install goes on with: $($bad -join ' | ')" }
    if ($after[-1] -notlike 'EVENT enter: *password*') { Fail "the install ends without asking to copy the password: $($after[-1])" }
    if (-not ($after | Where-Object { $_ -like '*Commands for later*' })) { Fail 'the password is not part of the summary at the end' }
    Remove-Item -Recurse -Force $dir
}

# WD-4: next to the public address the summary gives the console's
# address on the LAN, which works without port forwarding from inside;
# none when the two are the same.
function Test-SummaryNamesTheLanAddress {
    $dir = New-TestInstall
    # The installer's own certificate, named as the installer names it
    New-Item -ItemType Directory -Path (Join-Path $script:RUSTDESK_PATH 'ssl') -Force | Out-Null
    $crt = Join-Path $script:RUSTDESK_PATH 'ssl\neodesk.crt'
    $key = Join-Path $script:RUSTDESK_PATH 'ssl\neodesk.key'
    Set-Content -Path $crt, $key -Value 'pem'
    Set-Content -Path (Join-Path $script:CONSOLE_PATH '.env') -Value @('HTTPS_ENABLED=true', "SSL_CERT_PATH=$crt", "SSL_KEY_PATH=$key", 'PANEL_PUBLIC_HOST=203.0.113.7')
    function Get-LocalIP { '192.168.1.20' }
    $out = (Show-InstallSummary 6>&1 | Out-String -Width 4096)
    if (-not $out.Contains('https://203.0.113.7:5443')) { Fail "the summary does not give the public console address: $out" }
    if (-not $out.Contains('https://192.168.1.20:5443')) { Fail "the summary does not give the console address on the LAN: $out" }
    if ($out -notmatch 'self-signed') { Fail "the summary does not say the certificate is self-signed: $out" }
    function Get-LocalIP { '203.0.113.7' }
    $out = (Show-InstallSummary 6>&1 | Out-String -Width 4096)
    if ($out -match 'On your LAN') { Fail "the summary gives the same address twice: $out" }
    Remove-Item -Recurse -Force $dir
}

# WD-12: -ResetPassword gives admin a new password without questions:
# ADMIN_PASSWORD, else a random one; one shorter than 8 characters is
# refused. The password goes to the server on standard input.
function Test-ResetPasswordWithoutQuestions {
    $dir = New-TestInstall
    function Print-Header { }
    function Press-Enter { }
    function Auto-DetectPaths { }
    function Detect-Installation { }
    function Test-NodeEraInstall { $false }
    function Invoke-MenuChoose { param($Title, $Subtitle, $Items, $Returns) $script:menuAsked = $true; $script:MENU_CHOICE = '0' }
    function Invoke-ServerCommand { param([string[]]$Arguments, $InputText, $Binary) $script:resetCalls += , @(($Arguments -join ' '), "$InputText"); return 0 }
    $saved = $env:ADMIN_PASSWORD
    $script:RESET_PASSWORD_MODE = $true
    $script:menuAsked = $false
    $script:resetCalls = @()
    $env:ADMIN_PASSWORD = 'Given-Pass-123'
    $ok = Do-ResetPassword 6> $null
    if ("$ok" -ne 'True' -or $script:resetCalls.Count -ne 1 -or $script:resetCalls[0][0] -ne 'reset-password admin' -or $script:resetCalls[0][1] -ne 'Given-Pass-123') {
        Fail "-ResetPassword with ADMIN_PASSWORD: $ok, $(($script:resetCalls | ForEach-Object { $_ -join ' <- ' }) -join ' | ')"
    }
    $script:resetCalls = @()
    $env:ADMIN_PASSWORD = $null
    $ok = Do-ResetPassword 6> $null
    if ("$ok" -ne 'True' -or $script:resetCalls.Count -ne 1 -or $script:resetCalls[0][1] -notmatch '^[A-Za-z0-9]{16}$') { Fail "-ResetPassword without ADMIN_PASSWORD did not set a random one: $($script:resetCalls[0] -join ' <- ')" }
    $script:resetCalls = @()
    $env:ADMIN_PASSWORD = 'short'
    $ok = Do-ResetPassword 6> $null
    if ("$ok" -ne 'False' -or $script:resetCalls.Count -ne 0) { Fail 'a short ADMIN_PASSWORD was set' }
    if ($script:menuAsked) { Fail '-ResetPassword asked a question' }
    if ((Get-CommandsIn 'Main' 'Do-ResetPassword').Count -lt 2) { Fail 'Main does not run -ResetPassword' }

    # -ResetTwoFactor: reset-password --reset-two-factor admin, on a binary
    # that has it; an older one would take the switch for a second name, so
    # nothing runs and it says to update. -ResetTwoFactor alone means
    # -ResetPassword.
    $env:ADMIN_PASSWORD = 'Given-Pass-123'
    $script:RESET_TWO_FACTOR = $true
    $script:resetCalls = @()
    function Test-BinaryResetsTwoFactor { param($Path) $true }
    $ok = Do-ResetPassword 6> $null
    if ("$ok" -ne 'True' -or $script:resetCalls.Count -ne 1 -or $script:resetCalls[0][0] -ne 'reset-password --reset-two-factor admin' -or $script:resetCalls[0][1] -ne 'Given-Pass-123') {
        Fail "-ResetTwoFactor ran: $ok, $(($script:resetCalls | ForEach-Object { $_ -join ' <- ' }) -join ' | ')"
    }
    $script:resetCalls = @()
    function Test-BinaryResetsTwoFactor { param($Path) $false }
    $out = (Do-ResetPassword 6>&1 | Out-String -Width 4096)
    if ($script:resetCalls.Count -ne 0) { Fail "-ResetTwoFactor on an older binary ran: $($script:resetCalls[0][0])" }
    if ($out -notmatch 'cannot turn off two-factor' -or $out -notmatch '-Update') { Fail "-ResetTwoFactor on an older binary says: $out" }
    $script:RESET_TWO_FACTOR = $false
    $mode = @($ast.EndBlock.Statements | Where-Object {
            $_ -is [System.Management.Automation.Language.AssignmentStatementAst] -and $_.Left.Extent.Text -eq '$script:RESET_PASSWORD_MODE'
        })
    if ($mode.Count -ne 1 -or $mode[0].Right.Extent.Text -notmatch '\$ResetTwoFactor') { Fail '-ResetTwoFactor alone does not reset the password' }
    # The binary says it has the switch in its usage line (PR #43)
    $probe = Join-Path $dir 'probe.exe'
    Set-Content -Path $probe -Value 'Usage: neodesk-server reset-password [--reset-two-factor] [--verbose] [username]'
    Remove-Item function:Test-BinaryResetsTwoFactor -ErrorAction SilentlyContinue
    if (-not (Test-BinaryResetsTwoFactor -Path $probe)) { Fail 'a binary with --reset-two-factor does not count as one that has it' }
    Set-Content -Path $probe -Value 'Usage: neodesk-server reset-password [username]'
    if (Test-BinaryResetsTwoFactor -Path $probe) { Fail 'an older binary counts as one with --reset-two-factor' }
    $script:RESET_PASSWORD_MODE = $false
    $env:ADMIN_PASSWORD = $saved
    Remove-Item -Recurse -Force $dir
}

# WD-9 and WD-10: behind a reverse proxy (TRUST_PROXY) the console's
# firewall rules go again, the others stay. The uninstall takes every
# NeoDesk rule and the command line's Path entry, also when the data
# stays; the backups stay unless -Purge, and it says what stays.
function Test-FirewallRulesAndUninstall {
    $dir = New-TestInstall
    $envFile = Join-Path $script:CONSOLE_PATH '.env'
    Set-Content -Path $envFile -Value @('API_PORT=21121', 'CONSOLE_PORT=3375', 'HTTPS_ENABLED=true', 'HTTPS_PORT=5443', 'CDAP_ENABLED=N')
    Configure-Firewall *> $null
    $names = @($script:fwRules | ForEach-Object { $_.DisplayName })
    if ($names -notcontains 'NeoDesk - NeoDesk web console HTTPS') { Fail "the install opens no console rule: $($names -join ', ')" }
    Add-Content -Path $envFile -Value 'TRUST_PROXY=true'
    Configure-Firewall *> $null
    $names = @($script:fwRules | ForEach-Object { $_.DisplayName })
    if (@($names | Where-Object { $_ -like 'NeoDesk - NeoDesk web console*' }).Count -gt 0) { Fail "behind a reverse proxy the console's rules stay: $($names -join ', ')" }
    if ($names -notcontains 'NeoDesk - NeoDesk relay') { Fail "the proxy took the relay's rule: $($names -join ', ')" }

    function Print-Header { }
    function Stop-AllServices { }
    function Do-BackupSilent { }
    function Press-Enter { }
    $script:fwRules += [pscustomobject]@{ DisplayName = 'Other app'; Action = 'Allow'; Port = 8080; Proto = 'TCP' }
    $before = $script:machinePath
    $script:machinePath = "$before;$(Join-Path $script:RUSTDESK_PATH 'bin')"
    New-Item -ItemType Directory -Path $script:BACKUP_DIR -Force | Out-Null
    Set-Content -Path (Join-Path $script:BACKUP_DIR 'neodesk_backup_1.zip') -Value 'zip'
    $script:AUTO_MODE = $true
    $script:UNINSTALL_MODE = $true
    $script:PURGE_MODE = $false
    $script:fakeService = [pscustomobject]@{ PathName = 'x'; StartName = 'NT SERVICE\NeoDeskServer'; DisplayName = 'NeoDesk Server'; StartupType = 'Automatic'; Environment = @() }
    $out = (Do-Uninstall 6>&1 | Out-String -Width 4096)
    if ($script:fakeService) { Fail "the uninstall leaves the service: $(($script:scCalls | ForEach-Object { $_ -join ' ' }) -join ' | ')" }
    $names = @($script:fwRules | ForEach-Object { $_.DisplayName })
    if (@($names | Where-Object { $_ -like 'NeoDesk - *' }).Count -gt 0) { Fail "the uninstall leaves firewall rules: $($names -join ', ')" }
    if ($names -notcontains 'Other app') { Fail 'the uninstall took a rule of another program' }
    if ($script:machinePath -ne $before) { Fail "an uninstall that keeps the data leaves the Path as $($script:machinePath)" }
    if (-not (Test-Path $script:RUSTDESK_PATH) -or -not (Test-Path $script:BACKUP_DIR)) { Fail 'an uninstall without -Purge removed data' }
    if (-not $out.Contains($script:BACKUP_DIR) -or $out -notmatch 'Kept on this machine') { Fail "the uninstall does not say what stays: $out" }
    $script:PURGE_MODE = $true
    Do-Uninstall *> $null
    if (Test-Path $script:BACKUP_DIR) { Fail '-Purge leaves the backups' }
    $script:PURGE_MODE = $false
    $script:UNINSTALL_MODE = $false
    $script:machinePath = $before
    Remove-Item -Recurse -Force $dir -ErrorAction SilentlyContinue
}

# WD-5: Get-Help finds the help (a #Requires line right above it hides
# it); WD-15: downloads run without the progress bar, which slows
# Windows PowerShell 5.1 down; WD-13: no leftover "Enterprise" or C=PL.
function Test-HelpDownloadsAndWording {
    $help = $ast.GetHelpContent()
    if (-not $help -or $help.Synopsis -notmatch 'NeoDesk') { Fail 'Get-Help finds no help in neodesk.ps1' }
    elseif (-not ($help.Parameters.Keys | Where-Object { $_ -eq 'ResetPassword' })) { Fail 'the help has no -ResetPassword' }
    $statements = @($ast.EndBlock.Statements)
    $progress = @(0..($statements.Count - 1) | Where-Object {
            $statements[$_] -is [System.Management.Automation.Language.AssignmentStatementAst] -and
            $statements[$_].Left.Extent.Text -eq '$ProgressPreference' -and $statements[$_].Right.Extent.Text -eq "'SilentlyContinue'"
        })
    if ($progress.Count -eq 0) { Fail 'neodesk.ps1 downloads with the progress bar on' }
    $text = Get-Content $installer -Raw
    if ($text -cmatch 'Enterprise' -or $text -match 'C=PL') { Fail 'neodesk.ps1 still says Enterprise or C=PL' }
}

Test-HeaderAndColours
Test-MenuColours
Test-InstallMakesTheWindowsService
Test-BinaryRunsAsServiceProbe
Test-TaskBecomesTheService
Test-NssmServiceBecomesTheService
Test-TlsOnTheServiceCommandLine
Test-ConsoleHomeCopiesNoConsoleFiles
Test-MigrationMenusPassOverwrite
Test-PostgreSQLSwitchMovesTheWholeInstall
Test-ReleasesComeFromTheFork
Test-PostgreSQLAddressIsKept
Test-PgUriMeansPostgreSQL
Test-UpdateKeepsTheAdminsPorts
Test-ChannelAndDownloadErrors
Test-PanelPublicHost
Test-WindowsFirstHour
Test-CommandLineOnThePath
Test-NoUpgradeFromTheNodeConsole
Test-InstallerComesFromTheRelease
Test-UpdateMovesTheDefaultConsolePort
Test-FromSourceFindsTheCheckout
Test-UpdateAsksTheFork
Test-FirewallPorts
Test-CertificateFromTheBinary
Test-SecretsAreRandom
Test-DatabaseUrlOnly
Test-ReleaseSignatureOnWindows
Test-NoStableReleaseTakesTheNewestPrerelease
Test-MigrateCommand
Test-BackupUsesTheServersBackup
Test-FreshInstallStartsManaged
Test-FoldersAreLocked
Test-PasswordIsTheLastThingShown
Test-SummaryNamesTheLanAddress
Test-ResetPasswordWithoutQuestions
Test-FirewallRulesAndUninstall
Test-HelpDownloadsAndWording
if ($script:failures -gt 0) { exit 1 }
Write-Host 'Windows installer checks passed'
# The stand-ins set LASTEXITCODE, and a caller such as GitHub's pwsh step
# exits with it when the script ends without an exit of its own.
exit 0
