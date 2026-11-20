package mobile

import (
	"os"
	"runtime/debug"
	"strings"
)

// restoreEnv loads the real process environment. Go shared libraries loaded
// by bionic start with an empty environment (bionic passes no envp to
// initialisers), which breaks PATH lookups and child processes such as
// app_process that need BOOTCLASSPATH / ANDROID_ROOT.
func restoreEnv() {
	if os.Getenv("BOOTCLASSPATH") != "" && os.Getenv("PATH") != "" {
		return
	}
	if b, err := os.ReadFile("/proc/self/environ"); err == nil {
		for _, kv := range strings.Split(string(b), "\x00") {
			if k, v, ok := strings.Cut(kv, "="); ok && k != "" && os.Getenv(k) == "" {
				_ = os.Setenv(k, v)
			}
		}
	}
	defaults := map[string]string{
		"PATH":         "/product/bin:/apex/com.android.runtime/bin:/apex/com.android.art/bin:/system_ext/bin:/system/bin:/system/xbin:/odm/bin:/vendor/bin:/vendor/xbin",
		"ANDROID_ROOT": "/system",
		"ANDROID_DATA": "/data",
	}
	for k, v := range defaults {
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}

// SetEnv sets one environment variable of the Go runtime. Android calls it
// with the Java process environment before creating the engine: in some
// processes (e.g. started for a bound system service) /proc/self/environ is
// not readable, and child processes such as app_process need BOOTCLASSPATH.
func SetEnv(key, value string) {
	if key != "" {
		_ = os.Setenv(key, value)
	}
}

// SetCrashLog makes the Go runtime write fatal errors (panic message and all
// goroutine stacks) to path. Android drops a library's stderr, so without
// this a Go panic only shows up as an anonymous SIGABRT tombstone.
func SetCrashLog(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	return debug.SetCrashOutput(f, debug.CrashOptions{})
}
