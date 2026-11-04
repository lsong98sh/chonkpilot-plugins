param(
  [string]$Title = 'ChonkPilot 定时任务',
  [string]$Body = '',
  [string]$Url = '',
  [string]$AppID = 'ChonkPilot'
)
$ErrorActionPreference = 'Stop'
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] > $null
$template = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02)
$texts = $template.GetElementsByTagName('text')
$texts.Item(0).AppendChild($template.CreateTextNode($Title)) > $null
$texts.Item(1).AppendChild($template.CreateTextNode($Body)) > $null
if ($Url) {
  $template.DocumentElement.SetAttribute('activationType', 'protocol') > $null
  $template.DocumentElement.SetAttribute('launch', $Url) > $null
}
$toast = [Windows.UI.Notifications.ToastNotification]::new($template)
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier($AppID).Show($toast)
