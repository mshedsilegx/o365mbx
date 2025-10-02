//go:build windows

package main

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/windows/registry"
)

const longPathHelpText = `
To fix this, you must enable the "Win32 long paths" policy.
You can do this using the Group Policy Editor (gpedit.msc):
  1. Navigate to: Local Computer Policy -> Computer Configuration -> Administrative Templates -> System -> Filesystem
  2. Find and enable the "Enable Win32 long paths" option.
Or, you can use the Registry Editor (regedit.exe):
  1. Navigate to: HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Control\FileSystem
  2. Set the value of "LongPathsEnabled" (DWORD) to 1 (or create it if it does not exist).
A system restart may be required after making this change.`

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
		fatalMsg := "FATAL: Long path support is not enabled on this Windows system, but is required by o365mbx. The 'LongPathsEnabled' registry key was not found."
		log.Fatalf("%s\n%s", fatalMsg, longPathHelpText)
	} else if err != nil {
		// For other errors, we log and proceed.
		log.Debugf("Could not read LongPathsEnabled registry value: %v. Unable to verify long path support.", err)
		return
	}


	if val != 1 {
		fatalMsg := fmt.Sprintf("FATAL: Long path support is not enabled on this Windows system, but is required by o365mbx. The 'LongPathsEnabled' registry key is set to %d.", val)
		log.Fatalf("%s\n%s", fatalMsg, longPathHelpText)
	}

	log.Debug("Registry check passed: Long path support is enabled.")
}