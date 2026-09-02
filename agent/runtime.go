package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	runtimeAckPath        = "/api/v1/server/machine/runtimeTaskAck"
	runtimeMaxOutput      = 32 * 1024
	runtimeStreamMaxChunk = 8 * 1024
)

type RuntimeStatusResponse struct {
	RuntimeStream *RuntimeStreamConfig `json:"runtime_stream"`
}

type RuntimeStreamConfig struct {
	SessionID string `json:"session_id"`
	Service   string `json:"service"`
	ExpiresAt int64  `json:"expires_at"`
}

type RuntimeStreamChunk struct {
	SessionID   string `json:"session_id"`
	Service     string `json:"service"`
	Logs        string `json:"logs,omitempty"`
	Error       string `json:"error,omitempty"`
	CollectedAt int64  `json:"collected_at"`
	nextCursor  string
}

type RuntimeTask struct {
	TaskID    string `json:"task_id"`
	Service   string `json:"service"`
	Action    string `json:"action"`
	Lines     int    `json:"lines"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
}

type RuntimeResult struct {
	TaskID        string `json:"task_id"`
	Service       string `json:"service"`
	Action        string `json:"action"`
	Status        string `json:"status"`
	Message       string `json:"message"`
	ServiceStatus string `json:"service_status,omitempty"`
	ExitCode      int    `json:"exit_code"`
	Logs          string `json:"logs,omitempty"`
	ExecutedAt    int64  `json:"executed_at"`
}

func (c *Controller) setRuntimeStream(config *RuntimeStreamConfig) {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	if config == nil || strings.TrimSpace(config.SessionID) == "" || config.ExpiresAt <= time.Now().Unix() {
		c.runtimeStream = nil
		c.runtimeStreamCursor = ""
		return
	}
	normalizedService := normalizeRuntimeService(config.Service)
	if normalizedService == "" {
		c.runtimeStream = nil
		c.runtimeStreamCursor = ""
		return
	}
	if c.runtimeStream == nil || c.runtimeStream.SessionID != config.SessionID {
		c.runtimeStreamCursor = ""
	}
	copyConfig := *config
	copyConfig.Service = normalizedService
	c.runtimeStream = &copyConfig
}

func (c *Controller) currentRuntimeStream() (*RuntimeStreamConfig, string) {
	c.streamMu.RLock()
	defer c.streamMu.RUnlock()
	if c.runtimeStream == nil {
		return nil, ""
	}
	copyConfig := *c.runtimeStream
	return &copyConfig, c.runtimeStreamCursor
}

func (c *Controller) collectRuntimeStream() *RuntimeStreamChunk {
	config, cursor := c.currentRuntimeStream()
	if config == nil {
		return nil
	}
	if config.ExpiresAt <= time.Now().Unix() {
		c.setRuntimeStream(nil)
		return nil
	}

	unit := runtimeServiceUnit(config.Service)
	args := []string{"-u", unit, "--no-pager", "-o", "short-iso", "--show-cursor"}
	if cursor == "" {
		args = append(args, "-n", "80")
	} else {
		args = append(args, "--after-cursor", cursor)
	}
	output, _, err := runRuntimeCommand(5*time.Second, "journalctl", args...)
	logs, nextCursor := parseRuntimeJournalOutput(output)
	chunk := &RuntimeStreamChunk{
		SessionID:   config.SessionID,
		Service:     config.Service,
		Logs:        tailRuntimeLogOutput(logs, runtimeStreamMaxChunk),
		CollectedAt: time.Now().Unix(),
		nextCursor:  nextCursor,
	}
	if err != nil {
		chunk.Error = "读取实时日志失败: " + commandErrorMessage(err)
		chunk.nextCursor = ""
	}
	return chunk
}

func (c *Controller) acknowledgeRuntimeStream(chunk *RuntimeStreamChunk) {
	if chunk == nil || chunk.nextCursor == "" {
		return
	}
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	if c.runtimeStream != nil && c.runtimeStream.SessionID == chunk.SessionID {
		c.runtimeStreamCursor = chunk.nextCursor
	}
}

func parseRuntimeJournalOutput(output string) (string, string) {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	logLines := make([]string, 0, len(lines))
	cursor := ""
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "-- cursor:") {
			cursor = strings.TrimSpace(strings.TrimPrefix(trimmed, "-- cursor:"))
			continue
		}
		if trimmed == "-- No entries --" {
			continue
		}
		logLines = append(logLines, line)
	}
	return strings.TrimSpace(strings.Join(logLines, "\n")), cursor
}

func tailRuntimeLogOutput(output string, limit int) string {
	output = strings.TrimSpace(output)
	if len(output) <= limit {
		return output
	}
	tail := output[len(output)-limit:]
	if newline := strings.IndexByte(tail, '\n'); newline >= 0 {
		tail = tail[newline+1:]
	}
	return strings.TrimSpace(tail)
}

func (c *Controller) ProcessRuntimeTask(requestReload func() error) error {
	task := c.RuntimeTask()
	if task == nil || strings.TrimSpace(task.TaskID) == "" {
		return nil
	}

	resultPath := filepath.Join(filepath.Dir(c.ManagedFile), "runtime-task-result.json")
	result, loaded := loadRuntimeResult(resultPath, task.TaskID)
	if !loaded {
		result = executeRuntimeTask(*task, requestReload)
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if err := atomicWrite(resultPath, raw, 0o600); err != nil {
			return fmt.Errorf("persist runtime task result: %w", err)
		}
	}

	var response map[string]any
	if err := c.Client.Post(runtimeAckPath, result, &response); err != nil {
		return fmt.Errorf("acknowledge runtime task: %w", err)
	}
	return nil
}

func loadRuntimeResult(path, taskID string) (RuntimeResult, bool) {
	var result RuntimeResult
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &result) != nil {
		return RuntimeResult{}, false
	}
	return result, result.TaskID == taskID
}

func executeRuntimeTask(task RuntimeTask, requestReload func() error) RuntimeResult {
	result := RuntimeResult{
		TaskID:     strings.TrimSpace(task.TaskID),
		Service:    normalizeRuntimeService(task.Service),
		Action:     strings.ToLower(strings.TrimSpace(task.Action)),
		Status:     "failed",
		ExitCode:   -1,
		ExecutedAt: time.Now().Unix(),
	}
	if task.ExpiresAt > 0 && task.ExpiresAt <= time.Now().Unix() {
		result.Message = "运行任务已过期"
		return result
	}
	if result.Service == "" {
		result.Message = "不支持的服务"
		return result
	}

	lines := task.Lines
	if lines < 50 || lines > 300 {
		lines = 200
	}
	unit := runtimeServiceUnit(result.Service)
	result.ServiceStatus = serviceActiveState(unit)

	switch result.Action {
	case "logs":
		output, exitCode, err := runRuntimeCommand(15*time.Second, "journalctl", "-u", unit, "-n", strconv.Itoa(lines), "--no-pager", "-o", "short-iso")
		result.ExitCode = exitCode
		result.Logs = tailOutput(output, runtimeMaxOutput)
		if err != nil {
			result.Message = "读取运行日志失败: " + commandErrorMessage(err)
			return result
		}
		result.Status = "success"
		result.Message = fmt.Sprintf("已读取最近 %d 行日志", lines)
		return result
	case "status":
		output, exitCode, err := runRuntimeCommand(10*time.Second, "systemctl", "show", unit, "--no-pager", "--property=ActiveState,SubState,Result,ExecMainStatus,ExecMainStartTimestamp")
		result.ExitCode = exitCode
		result.Logs = tailOutput(output, runtimeMaxOutput)
		if err != nil {
			result.Message = "读取服务状态失败: " + commandErrorMessage(err)
			return result
		}
		result.Status = "success"
		result.Message = "服务状态读取完成"
		return result
	case "reload", "restart":
		if result.Service == "ravel" {
			if requestReload == nil {
				result.Message = "Ravel 重载通道不可用"
				return result
			}
			if err := requestReload(); err != nil {
				result.Message = "Ravel 重载请求失败: " + err.Error()
				return result
			}
			result.Status = "success"
			result.ExitCode = 0
			result.Message = "Ravel 已接收配置重载请求"
			return result
		}
	case "start", "stop":
		if result.Service != "gost" {
			result.Message = "控制进程不允许远程启动或停止"
			return result
		}
	default:
		result.Message = "不支持的运行操作"
		return result
	}

	output, exitCode, err := runRuntimeCommand(20*time.Second, "systemctl", result.Action, unit)
	result.ExitCode = exitCode
	result.Logs = tailOutput(output, runtimeMaxOutput)
	result.ServiceStatus = serviceActiveState(unit)
	if err != nil {
		result.Message = fmt.Sprintf("%s %s 失败: %s", result.Service, result.Action, commandErrorMessage(err))
		return result
	}
	result.Status = "success"
	result.Message = fmt.Sprintf("%s %s 完成", result.Service, result.Action)
	return result
}

func normalizeRuntimeService(service string) string {
	switch strings.ToLower(strings.TrimSpace(service)) {
	case "ravel", "v2node":
		return "ravel"
	case "gost":
		return "gost"
	default:
		return ""
	}
}

func runtimeServiceUnit(service string) string {
	if service == "gost" {
		return "gost.service"
	}
	if serviceUnitExists("ravel.service") {
		return "ravel.service"
	}
	return "v2node.service"
}

func serviceUnitExists(unit string) bool {
	output, _, err := runRuntimeCommand(5*time.Second, "systemctl", "show", unit, "--property=LoadState", "--value")
	return err == nil && strings.TrimSpace(output) == "loaded"
}

func serviceActiveState(unit string) string {
	output, _, err := runRuntimeCommand(5*time.Second, "systemctl", "is-active", unit)
	state := strings.TrimSpace(output)
	if state == "" && err != nil {
		return "unknown"
	}
	return state
}

func runRuntimeCommand(timeout time.Duration, name string, args ...string) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
	command.Env = append(command.Environ(), "LC_ALL=C", "LANG=C")
	output, err := command.CombinedOutput()
	exitCode := 0
	if err != nil {
		exitCode = -1
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			exitCode = exitError.ExitCode()
		}
	}
	if ctx.Err() == context.DeadlineExceeded {
		return string(output), exitCode, fmt.Errorf("command timed out")
	}
	return string(output), exitCode, err
}

func commandErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimSpace(err.Error())
}

func tailOutput(output string, limit int) string {
	if len(output) <= limit {
		return strings.TrimSpace(output)
	}
	return strings.TrimSpace(output[len(output)-limit:])
}
