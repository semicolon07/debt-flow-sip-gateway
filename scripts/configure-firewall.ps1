[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [string]$RemoteAddress = "LocalSubnet",
    [string]$RuleGroup = "Debt Flow SIP Gateway"
)

$ErrorActionPreference = "Stop"
if (-not $PSCmdlet.ShouldProcess($RuleGroup, "Create private-profile inbound firewall rules")) { return }

Get-NetFirewallRule -Group $RuleGroup -ErrorAction SilentlyContinue | Remove-NetFirewallRule
New-NetFirewallRule -DisplayName "Debt Flow SIP UDP" -Group $RuleGroup -Direction Inbound -Action Allow -Profile Private -Protocol UDP -LocalPort 5060 -RemoteAddress $RemoteAddress | Out-Null
New-NetFirewallRule -DisplayName "Debt Flow SIP TCP" -Group $RuleGroup -Direction Inbound -Action Allow -Profile Private -Protocol TCP -LocalPort 5060 -RemoteAddress $RemoteAddress | Out-Null
New-NetFirewallRule -DisplayName "Debt Flow RTP" -Group $RuleGroup -Direction Inbound -Action Allow -Profile Private -Protocol UDP -LocalPort 20000-21999 -RemoteAddress $RemoteAddress | Out-Null
New-NetFirewallRule -DisplayName "Debt Flow ICE" -Group $RuleGroup -Direction Inbound -Action Allow -Profile Private -Protocol UDP -LocalPort 22000 -RemoteAddress $RemoteAddress | Out-Null
New-NetFirewallRule -DisplayName "Debt Flow TURN UDP" -Group $RuleGroup -Direction Inbound -Action Allow -Profile Private -Protocol UDP -LocalPort 3478 -RemoteAddress $RemoteAddress | Out-Null
New-NetFirewallRule -DisplayName "Debt Flow TURN TCP" -Group $RuleGroup -Direction Inbound -Action Allow -Profile Private -Protocol TCP -LocalPort 3478 -RemoteAddress $RemoteAddress | Out-Null
New-NetFirewallRule -DisplayName "Debt Flow TURN relay" -Group $RuleGroup -Direction Inbound -Action Allow -Profile Private -Protocol UDP -LocalPort 30000-30999 -RemoteAddress $RemoteAddress | Out-Null

