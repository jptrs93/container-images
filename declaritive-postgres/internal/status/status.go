package status

import (
	"fmt"
	"os"
	"strings"
	"time"
)

func Write(directory, value string) error {
	if err := os.MkdirAll(directory, 0750); err != nil {
		return err
	}
	contents := fmt.Sprintf("%s %s\n", time.Now().UTC().Format(time.RFC3339), value)
	temporary, err := os.CreateTemp(directory, ".status-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0644); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, directory+"/status")
}

func Healthy(directory string) bool {
	return Read(directory) == "healthy"
}

func Read(directory string) string {
	contents, err := os.ReadFile(directory + "/status")
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(contents))
	if len(fields) != 2 {
		return ""
	}
	return fields[1]
}
