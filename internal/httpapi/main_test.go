package httpapi

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestMain сериализует интеграционные тесты пакета с другими пакетами,
// использующими ту же TEST_DATABASE_URL (см. internal/postgres).
func TestMain(m *testing.M) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		os.Exit(m.Run())
	}
	lock, err := os.OpenFile(filepath.Join(os.TempDir(), "hookrelay-it.lock"),
		os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		panic(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	os.Exit(code)
}
