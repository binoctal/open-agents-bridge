package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/spf13/cobra"
)

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "View bridge logs",
	Run:   runLogs,
}

var (
	logLines  int
	logFollow bool
)

func init() {
	logsCmd.Flags().IntVarP(&logLines, "lines", "n", 50, "Number of lines to show")
	logsCmd.Flags().BoolVarP(&logFollow, "follow", "f", false, "Follow log output")
}

func runLogs(cmd *cobra.Command, args []string) {
	logDir := getLogDir()
	logFile := findLatestLog(logDir)

	if logFile == "" {
		fmt.Println("No log files found in", logDir)
		return
	}

	if logFollow {
		tailFollow(logFile)
	} else {
		tailLines(logFile, logLines)
	}
}

func getLogDir() string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("APPDATA"), "open-agents-bridge", "logs")
	case "darwin":
		return filepath.Join(os.Getenv("HOME"), "Library", "Logs", "open-agents-bridge")
	default:
		return filepath.Join(os.Getenv("HOME"), ".open-agents-bridge", "logs")
	}
}

func findLatestLog(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	var logs []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".log" {
			logs = append(logs, filepath.Join(dir, e.Name()))
		}
	}

	if len(logs) == 0 {
		return ""
	}

	sort.Slice(logs, func(i, j int) bool {
		fi, _ := os.Stat(logs[i])
		fj, _ := os.Stat(logs[j])
		return fi.ModTime().After(fj.ModTime())
	})

	return logs[0]
}

func tailLines(path string, n int) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}

	for _, line := range lines {
		fmt.Println(line)
	}
}

func tailFollow(path string) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer f.Close()

	f.Seek(0, 2) // Seek to end

	scanner := bufio.NewScanner(f)
	for {
		for scanner.Scan() {
			fmt.Println(scanner.Text())
		}
	}
}
