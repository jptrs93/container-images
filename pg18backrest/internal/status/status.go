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
	return os.WriteFile(directory+"/status", []byte(contents), 0644)
}

func Healthy(directory string) bool {
	contents, err := os.ReadFile(directory + "/status")
	return err == nil && strings.HasSuffix(strings.TrimSpace(string(contents)), " healthy")
}
