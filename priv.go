package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// errAuthCancelled is returned when the user dismisses the password dialog.
var errAuthCancelled = fmt.Errorf("authentication cancelled")

// runPrivileged runs the given command as root. Already-root → exec directly.
// Otherwise pipes a password (collected via the in-app prompt) to sudo -S.
// The password is cached for the session; on auth failure the cache is cleared.
func (u *UI) runPrivileged(args ...string) (string, error) {
	logf("priv: invoke %s", strings.Join(args, " "))
	if os.Geteuid() == 0 {
		logf("priv: euid=0, executing directly")
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = append(os.Environ(), "LC_ALL=C")
		out, err := cmd.CombinedOutput()
		s := strings.TrimSpace(string(out))
		logf("priv: result err=%v output=%q", err, truncate(s, 400))
		return s, err
	}
	for attempt := 1; ; attempt++ {
		pw, ok := u.askPassword(args)
		if !ok {
			logf("priv: user cancelled password prompt for: %s", strings.Join(args, " "))
			return "", errAuthCancelled
		}
		sudoArgs := append([]string{"-S", "-k", "-p", "", "--"}, args...)
		logf("priv: attempt %d via sudo (cached_pw=%v)", attempt, pw != "")
		cmd := exec.Command("sudo", sudoArgs...)
		cmd.Stdin = strings.NewReader(pw + "\n")
		cmd.Env = append(os.Environ(), "LC_ALL=C")
		out, err := cmd.CombinedOutput()
		txt := strings.TrimSpace(string(out))
		if err != nil && isAuthError(txt) {
			logf("priv: authentication failed (attempt %d), clearing cache", attempt)
			u.pwMu.Lock()
			u.cachedPw = ""
			u.lastAuthErr = "Incorrect password."
			u.pwMu.Unlock()
			continue
		}
		txt = stripSudoNoise(txt)
		if err != nil {
			logf("priv: command failed err=%v output=%q", err, truncate(txt, 400))
		} else {
			logf("priv: command succeeded output=%q", truncate(txt, 400))
		}
		return txt, err
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func isAuthError(out string) bool {
	low := strings.ToLower(out)
	return strings.Contains(low, "incorrect password") ||
		strings.Contains(low, "sorry, try again") ||
		strings.Contains(low, "authentication failure") ||
		strings.Contains(low, "3 incorrect password attempts")
}

func stripSudoNoise(s string) string {
	lines := strings.Split(s, "\n")
	kept := lines[:0]
	for _, ln := range lines {
		trim := strings.TrimSpace(ln)
		if trim == "" || strings.HasPrefix(trim, "Password:") || strings.HasPrefix(trim, "Sorry,") {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// mountAt builds fstype-appropriate options so the current user has read/write.
// Foreign filesystems (NTFS / FAT / exFAT / ISO / HFS+) honor uid/gid/umask at
// mount time; native UNIX filesystems use real permissions, so we chown the
// mountpoint to the invoking user after the mount succeeds.
func (u *UI) mountAt(dev, target, fstype string) (string, error) {
	uid := os.Getuid()
	gid := os.Getgid()
	if sudoUID := os.Getenv("SUDO_UID"); sudoUID != "" {
		fmt.Sscanf(sudoUID, "%d", &uid)
	}
	if sudoGID := os.Getenv("SUDO_GID"); sudoGID != "" {
		fmt.Sscanf(sudoGID, "%d", &gid)
	}

	var opts []string
	var driverType string // explicit -t value, empty = let mount pick
	needChown := false
	switch strings.ToLower(fstype) {
	case "ntfs", "ntfs3":
		// Force the read/write kernel driver — without -t, mount may pick the
		// legacy read-only "ntfs" driver and reject modern options.
		driverType = "ntfs3"
		opts = []string{fmt.Sprintf("uid=%d,gid=%d,umask=022,windows_names", uid, gid)}
	case "vfat", "msdos":
		opts = []string{fmt.Sprintf("uid=%d,gid=%d,umask=022,flush", uid, gid)}
	case "exfat":
		opts = []string{fmt.Sprintf("uid=%d,gid=%d,umask=022", uid, gid)}
	case "iso9660", "udf":
		opts = []string{fmt.Sprintf("uid=%d,gid=%d,umask=022,ro", uid, gid)}
	case "hfsplus", "hfs":
		opts = []string{fmt.Sprintf("uid=%d,gid=%d,umask=022,force", uid, gid)}
	default:
		// ext2/3/4, btrfs, xfs, f2fs, etc. — native Unix permissions.
		needChown = true
	}

	buildArgs := func(extraOpt string) []string {
		a := []string{"mount"}
		if driverType != "" {
			a = append(a, "-t", driverType)
		}
		o := append([]string(nil), opts...)
		if extraOpt != "" {
			o = append(o, extraOpt)
		}
		for _, x := range o {
			a = append(a, "-o", x)
		}
		return append(a, dev, target)
	}

	args := buildArgs("")
	logf("mount: dev=%s target=%s fstype=%q driver=%q opts=%v needChown=%v",
		dev, target, fstype, driverType, opts, needChown)

	out, err := u.runPrivileged(args...)

	// ntfs3 writes the actual failure reason to the kernel ring buffer rather
	// than to stderr. Grab the tail so the user has a real diagnostic, then
	// retry once with `force` (which lets ntfs3 mount R/W over a dirty journal
	// / clear hibernation state on the next clean unmount).
	if err != nil && driverType == "ntfs3" {
		u.logDmesgTail("ntfs3")
		logf("mount: NTFS first attempt failed, retrying with -o force")
		args = buildArgs("force")
		out2, err2 := u.runPrivileged(args...)
		if err2 == nil {
			out, err = out2, nil
		} else {
			u.logDmesgTail("ntfs3")
			out = strings.TrimSpace(out + "\n" + out2)
			err = err2
		}
	}

	if err != nil {
		return out, err
	}
	if needChown {
		who := fmt.Sprintf("%d:%d", uid, gid)
		if out2, err2 := u.runPrivileged("chown", who, target); err2 != nil {
			return strings.TrimSpace(out + "\n" + out2), err2
		}
	}
	return out, nil
}

// logDmesgTail fetches the last few kernel-ring lines mentioning the given
// keyword (privileged, since dmesg is root-only by default) and writes them
// into the app log.
func (u *UI) logDmesgTail(keyword string) {
	out, err := u.runPrivileged("sh", "-c",
		fmt.Sprintf("dmesg -T 2>/dev/null | grep -i %q | tail -n 20", keyword))
	if err != nil {
		logf("dmesg: could not read kernel log: %v", err)
		return
	}
	if strings.TrimSpace(out) == "" {
		logf("dmesg: no %q messages found in the kernel ring buffer", keyword)
		return
	}
	logf("dmesg tail (filter=%q):\n%s", keyword, out)
}

func (u *UI) umount(dev string) (string, error) {
	logf("umount: dev=%s", dev)
	return u.runPrivileged("umount", dev)
}
