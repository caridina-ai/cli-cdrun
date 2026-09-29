package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// State is private to the account and integrity level that launched the broker.
func stateRoot() string {
	suffix := "cdrun"
	if windows.GetCurrentProcessToken().IsElevated() {
		suffix += "-elevated"
	}
	return filepath.Join(os.Getenv("LOCALAPPDATA"), suffix)
}

func secureStateRoot() error {
	if !filepath.IsAbs(os.Getenv("LOCALAPPDATA")) {
		return errors.New("LOCALAPPDATA must be an absolute path")
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	label := "ME"
	if windows.GetCurrentProcessToken().IsElevated() {
		label = "HI"
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;" + u.User.Sid.String() + ")S:(ML;OICI;NRNW;;;" + label + ")")
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(stateRoot())
	if err != nil {
		return err
	}
	err = windows.CreateDirectory(p, &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd})
	if err != nil && err != windows.ERROR_ALREADY_EXISTS {
		return err
	}
	info, err := os.Lstat(stateRoot())
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("state directory must not be a link")
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	sacl, _, err := sd.SACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(stateRoot(), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION, nil, nil, dacl, sacl)
}

func processBirth(h windows.Handle) (uint64, error) {
	var birth, exit, kernel, user windows.Filetime
	err := windows.GetProcessTimes(h, &birth, &exit, &kernel, &user)
	return uint64(birth.HighDateTime)<<32 | uint64(birth.LowDateTime), err
}

// Keep this handle open across shutdown, so PID recycling cannot change the target.
func openVerified(e endpoint, terminate bool) (windows.Handle, error) {
	access := uint32(windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.SYNCHRONIZE)
	if terminate {
		access |= windows.PROCESS_TERMINATE
	}
	h, err := windows.OpenProcess(access, false, uint32(e.PID))
	if err != nil {
		return 0, err
	}
	fail := func(err error) (windows.Handle, error) { windows.CloseHandle(h); return 0, err }
	birth, err := processBirth(h)
	if err != nil {
		return fail(err)
	}
	if birth != e.Created || birth == 0 {
		return fail(errors.New("stale session: process creation time changed"))
	}
	var path [32768]uint16
	n := uint32(len(path))
	if err = windows.QueryFullProcessImageName(h, 0, &path[0], &n); err != nil {
		return fail(err)
	}
	if !strings.EqualFold(windows.UTF16ToString(path[:n]), e.Executable) {
		return fail(errors.New("session executable mismatch"))
	}
	if result, err := windows.WaitForSingleObject(h, 0); err != nil || result != uint32(windows.WAIT_TIMEOUT) {
		return fail(errors.New("session process has exited"))
	}
	return h, nil
}

func validateEndpoint(e endpoint, pid int) error {
	if e.PID != pid || pid <= 0 || e.Created == 0 || !filepath.IsAbs(e.Executable) || len(e.Token) != 64 {
		return errors.New("invalid session identity")
	}
	u, err := url.Parse(e.URL)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid session endpoint")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return errors.New("invalid session port")
	}
	h, err := openVerified(e, false)
	if err != nil {
		return fmt.Errorf("inactive cdrun PID %d: %w", pid, err)
	}
	return windows.CloseHandle(h)
}

// Remove only records whose process is provably gone or has a different birth time.
func cleanStaleEndpoints() {
	paths, _ := filepath.Glob(filepath.Join(stateRoot(), "*.json"))
	for _, path := range paths {
		pid, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(path), ".json"))
		if err != nil || pid <= 0 {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var e endpoint
		if json.Unmarshal(data, &e) != nil || e.PID != pid {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
		if err == windows.ERROR_INVALID_PARAMETER {
			_ = os.Remove(path)
			continue
		}
		if err != nil {
			continue
		}
		birth, birthErr := processBirth(h)
		windows.CloseHandle(h)
		if birthErr == nil && birth != e.Created {
			_ = os.Remove(path)
		}
	}
}
