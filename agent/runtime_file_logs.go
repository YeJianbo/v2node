package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func runtimeOpenRC() bool { _, err := os.Stat("/run/openrc"); return err == nil }

func runtimeOpenRCService(service string) string {
	if service == "gost" {
		return "gost"
	}
	if _, err := os.Stat("/etc/init.d/ravel"); err == nil {
		return "ravel"
	}
	return "v2node"
}

func runtimeLogPath(service string) string {
	if service == "gost" {
		return "/var/log/gost.log"
	}
	return "/var/log/" + runtimeOpenRCService(service) + ".log"
}

type fileLogCursor struct {
	Offset int64
	Anchor string
}

// The anchor detects truncation and rotation without platform-specific inode APIs.
func readRuntimeFile(path, cursor string, limit int) (string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", fmt.Errorf("日志文件 %s 不可读: %w", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", "", err
	}
	anchor := func(offset int64) string {
		start := offset - 64
		if start < 0 {
			start = 0
		}
		data := make([]byte, offset-start)
		if _, err := f.ReadAt(data, start); err != nil && err != io.EOF {
			return ""
		}
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:])
	}
	var position fileLogCursor
	valid := json.Unmarshal([]byte(cursor), &position) == nil && position.Offset >= 0 && position.Offset <= st.Size() && position.Anchor == anchor(position.Offset)
	if !valid {
		position.Offset = st.Size() - int64(limit)
		if position.Offset < 0 {
			position.Offset = 0
		}
	}
	data := make([]byte, min(int64(limit), st.Size()-position.Offset))
	n, err := f.ReadAt(data, position.Offset)
	if err != nil && err != io.EOF {
		return "", "", err
	}
	position.Offset += int64(n)
	position.Anchor = anchor(position.Offset)
	next, _ := json.Marshal(position)
	return string(data[:n]), string(next), nil
}

func readRuntimeLogs(service string, lines int) (string, int, error) {
	if !runtimeOpenRC() && !runtimeHasFileLog(service) {
		return runRuntimeCommand(15*time.Second, "journalctl", "-u", runtimeServiceUnit(service), "-n", fmt.Sprint(lines), "--no-pager", "-o", "short-iso")
	}
	logs, _, err := readRuntimeFile(runtimeLogPath(service), "", runtimeMaxOutput)
	if err != nil {
		return "", -1, err
	}
	entries := strings.Split(strings.TrimSpace(logs), "\n")
	if len(entries) > lines {
		entries = entries[len(entries)-lines:]
	}
	return strings.Join(entries, "\n"), 0, nil
}

func runtimeServiceCommand(service, action string) (string, int, error) {
	if runtimeOpenRC() {
		return runRuntimeCommand(20*time.Second, "rc-service", runtimeOpenRCService(service), action)
	}
	return runRuntimeCommand(20*time.Second, "systemctl", action, runtimeServiceUnit(service))
}

func runtimeHasFileLog(service string) bool {
	if service != "gost" {
		return false
	}
	_, err := os.Stat(runtimeLogPath(service))
	return err == nil
}
