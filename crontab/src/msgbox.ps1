param(
  [string]$Title = 'ChonkPilot 定时任务',
  [string]$Body = ''
)
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Windows.Forms
[System.Windows.Forms.MessageBox]::Show($Body, $Title, 'OK', 'Information') > $null
