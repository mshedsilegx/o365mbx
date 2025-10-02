//go:build windows

package main

import (
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/windows/registry"
)

// checkLongPathSupport verifies that the OS can handle long file paths by checking the Windows registry.
func checkLongPathSupport() {
	log.Debug("Running on Windows, checking registry for long path support...")

	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\FileSystem`, registry.QUERY_VALUE)
	if err != nil {
		// If the key can't be opened, we can't verify.
		// This is unlikely, but we log it and proceed with caution.
		log.Debugf("Could not open FileSystem registry key: %v. Unable to verify long path support.", err)
		return
	}
	defer key.Close()

	val, _, err := key.GetIntegerValue("LongPathsEnabled")
	// If the value doesn't exist, it's disabled by default.
	if err == registry.ErrNotExist {
		log.Fatalf(`
FATAL: Long path support is not enabled on this Windows system, but is required by o365mbx. The 'LongPathsEnabled' registry key was not found.
To fix this, you must enable the "Win32 long paths" policy.
You can do this using the Group Policy Editor (gpedit.msc):
  1. Navigate to: Local Computer Policy -> Computer Configuration -> Administrative Templates -> System -> Filesystem
  2. Find and enable the "Enable Win32 long paths" option.
Or, you can use the Registry Editor (regedit.exe):
  1. Navigate to: HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Control\FileSystem
  2. Create a new DWORD (32-bit) Value named "LongPathsEnabled" and set it to 1.
A system restart may be required after making this change.`)
	} else if err != nil {
		// For other errors, we log and proceed.
		log.Debugf("Could not read LongPathsEnabled registry value: %v. Unable to verify long path support.", err)
		return
	}


	if val != 1 {
		log.Fatalf(`
FATAL: Long path support is not enabled on this Windows system, but is required by o365mbx. The 'LongPathsEnabled' registry key is set to 0.
To fix this, you must enable the "Win32 long paths" policy.
You can do this using the Group Policy Editor (gpedit.msc):
  1. Navigate to: Local Computer Policy -> Computer Configuration -> Administrative Templates -> System -> Filesystem
  2. Find and enable the "Enable Win32 long paths" option.
Or, you can use the Registry Editor (regedit.exe):
  1. Navigate to: HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Control\FileSystem
  2. Set the value of "LongPathsEnabled" (DWORD) to 1.
A system restart may be required after making this change.`)
	}

	log.Debug("Registry check passed: Long path support is enabled.")
}