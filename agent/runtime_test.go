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

func TestParseRuntimeJournalOutputSeparatesCursor(t *testing.T) {
	logs, cursor := parseRuntimeJournalOutput("first line\nsecond line\n-- cursor: s=abc;i=2\n")
	if logs != "first line\nsecond line" {
		t.Fatalf("unexpected logs: %q", logs)
	}
	if cursor != "s=abc;i=2" {
		t.Fatalf("unexpected cursor: %q", cursor)
	}
}

func TestTailRuntimeLogOutputStartsAtCompleteLine(t *testing.T) {
	got := tailRuntimeLogOutput("first line\nsecond line\nthird line", 23)
	if got != "second line\nthird line" {
		t.Fatalf("unexpected log tail: %q", got)
	}
}

func TestSetRuntimeStreamRejectsUnknownService(t *testing.T) {
	controller := &Controller{}
	controller.setRuntimeStream(&RuntimeStreamConfig{
		SessionID: "stream-1",
		Service:   "shell",
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	config, _ := controller.currentRuntimeStream()
	if config != nil {
		t.Fatalf("unknown service should not create a stream: %#v", config)
	}
}

func TestSetRuntimeStreamResetsCursorForNewSession(t *testing.T) {
	controller := &Controller{
		runtimeStream:       &RuntimeStreamConfig{SessionID: "old", Service: "ravel"},
		runtimeStreamCursor: "old-cursor",
	}
	controller.setRuntimeStream(&RuntimeStreamConfig{
		SessionID: "new",
		Service:   "gost",
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	config, cursor := controller.currentRuntimeStream()
	if config == nil || config.SessionID != "new" || config.Service != "gost" {
		t.Fatalf("unexpected stream config: %#v", config)
	}
	if cursor != "" {
		t.Fatalf("cursor was not reset: %q", cursor)
	}
}
