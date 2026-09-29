param([string]$Action, [string]$A, [string]$B, [string]$Relative)
Add-Type @'
using System;
using System.Runtime.InteropServices;
public class Mouse {
  [DllImport("user32.dll")] public static extern bool SetCursorPos(int x, int y);
  [DllImport("user32.dll")] public static extern void mouse_event(uint flags, uint dx, uint dy, uint data, UIntPtr extra);
}
'@
if ($Action -eq "move") {
  if ($Relative -eq "true") { [Mouse]::mouse_event(0x0001, [uint32]$A, [uint32]$B, 0, [UIntPtr]::Zero) }
  else { [Mouse]::SetCursorPos([int]$A, [int]$B) }
} elseif ($Action -eq "button") {
  $flags = if ($A -eq "left") { 0x0002 } elseif ($A -eq "right") { 0x0008 } else { 0x0020 }
  if ($B -eq "up") { $flags = $flags -bor 0x0010 }
  [Mouse]::mouse_event($flags, 0, 0, 0, [UIntPtr]::Zero)
} elseif ($Action -eq "scroll") {
  [Mouse]::mouse_event(0x0800, 0, 0, [uint32]$B, [UIntPtr]::Zero)
}
