package agent

import (
	"strings"
	"testing"
	"time"
)

func TestExecuteRuntimeTaskRejectsUnknownService(t *testing.T) {
	result := executeRuntimeTask(RuntimeTask{
		TaskID:    "task-1",
		Service:   "shell",
		Action:    "logs",
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	}, nil)
	if result.Status != "failed" || result.Message != "不支持的服务" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestExecuteRuntimeTaskRejectsRavelStop(t *testing.T) {
	result := executeRuntimeTask(RuntimeTask{
		TaskID:    "task-2",
		Service:   "ravel",
		Action:    "stop",
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	}, nil)
	if result.Status != "failed" || !strings.Contains(result.Message, "不允许") {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestExecuteRuntimeTaskDoesNotRunExpiredTask(t *testing.T) {
	called := false
	result := executeRuntimeTask(RuntimeTask{
		TaskID:    "task-3",
		Service:   "ravel",
		Action:    "reload",
		ExpiresAt: time.Now().Add(-time.Second).Unix(),
	}, func() error {
		called = true
		return nil
	})
	if called || result.Status != "failed" || result.Message != "运行任务已过期" {
		t.Fatalf("unexpected result: %#v, reload called: %v", result, called)
	}
}

func TestTailOutputKeepsNewestBytes(t *testing.T) {
	if got := tailOutput("123456789", 4); got != "6789" {
		t.Fatalf("tailOutput() = %q", got)
	}
}
