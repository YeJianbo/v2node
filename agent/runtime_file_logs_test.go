package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeFileCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.log")
	write := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("0123456789")
	data, cursor, err := readRuntimeFile(path, "", 4)
	if err != nil || data != "6789" {
		t.Fatalf("initial: %q %v", data, err)
	}
	data, cursor, err = readRuntimeFile(path, cursor, 4)
	if err != nil || data != "" {
		t.Fatalf("duplicate: %q %v", data, err)
	}
	write("0123456789abcd")
	data, cursor, err = readRuntimeFile(path, cursor, 4)
	if err != nil || data != "abcd" {
		t.Fatalf("append: %q %v", data, err)
	}
	write("new")
	data, cursor, err = readRuntimeFile(path, cursor, 4)
	if err != nil || data != "new" {
		t.Fatalf("truncate: %q %v", data, err)
	}
	write("replacement")
	data, _, err = readRuntimeFile(path, cursor, 4)
	if err != nil || data != "ment" {
		t.Fatalf("replace: %q %v", data, err)
	}
}

func TestRuntimeFileMissing(t *testing.T) {
	if _, _, err := readRuntimeFile(filepath.Join(t.TempDir(), "missing"), "", 4); err == nil {
		t.Fatal("missing log accepted")
	}
}

func TestRuntimeLogBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bounded.log")
	w, err := newBoundedRuntimeLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err = w.Write(make([]byte, 8*1024*1024)); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write([]byte("after rotation\n")); err != nil {
		t.Fatal(err)
	}
	data, _, err := readRuntimeFile(path, "", 1024)
	if err != nil || data != "after rotation\n" {
		t.Fatalf("%q %v", data, err)
	}
	st, err := os.Stat(path + ".1")
	if err != nil || st.Size() != 8*1024*1024 {
		t.Fatalf("backup: %v %v", st, err)
	}
}
