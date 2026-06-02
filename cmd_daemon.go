package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

const (
	pidFileName       = "gateway.pid"
	preferredLogFile  = "/var/log/ccg.log"
	readyTimeout      = 120 * time.Second
	readyPollInterval = 2 * time.Second
	stopGraceTimeout  = 10 * time.Second
)

// ccgDir returns ~/.ccg and ensures the directory exists.
func ccgDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	dir := filepath.Join(home, ".ccg")
	_ = os.MkdirAll(dir, 0700)
	return dir
}

// daemonPIDPath returns the path of the PID file.
func daemonPIDPath() string {
	return filepath.Join(ccgDir(), pidFileName)
}

// daemonLogPath resolves the log file path:
//  1. CCG_LOG_FILE env var
//  2. /var/log/ccg.log (if writable)
//  3. ~/.ccg/ccg.log
func daemonLogPath() string {
	if v := os.Getenv("CCG_LOG_FILE"); v != "" {
		return v
	}
	f, err := os.OpenFile(preferredLogFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err == nil {
		f.Close()
		return preferredLogFile
	}
	return filepath.Join(ccgDir(), "ccg.log")
}

// readPID reads the PID file, checks whether the process is alive, removes
// the file if the process is dead, and returns the live PID (or 0).
func readPID(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	if err := syscall.Kill(pid, 0); err != nil {
		_ = os.Remove(path)
		return 0
	}
	return pid
}

// ppidOf returns the parent PID of the given process.
func ppidOf(pid int) int {
	if pid == os.Getpid() {
		return os.Getppid()
	}
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			return 0
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "PPid:") {
				ppid, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
				return ppid
			}
		}
		return 0
	}
	// macOS / other POSIX: use ps
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	ppid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return ppid
}

// guardSelfKill returns true if targetPID is an ancestor of the current
// process. Prevents a Claude subprocess from stopping its own parent gateway.
func guardSelfKill(targetPID int) bool {
	cur := os.Getpid()
	for depth := 0; depth < 64; depth++ {
		ppid := ppidOf(cur)
		if ppid <= 1 || ppid == cur {
			break
		}
		if ppid == targetPID {
			fmt.Fprintf(os.Stderr,
				"ERROR: refusing to stop ccg PID %d — this process is its descendant.\n"+
					"  Run 'ccg stop' from a host shell outside the gateway-managed session.\n",
				targetPID)
			return true
		}
		cur = ppid
	}
	return false
}

// healthURL converts a listenAddr (e.g. ":8080", "0.0.0.0:8080") to a
// localhost health-check URL.
func healthURL(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "http://localhost:8080/health"
	}
	if host == "" || host == "0.0.0.0" {
		host = "localhost"
	}
	return fmt.Sprintf("http://%s:%s/health", host, port)
}

// waitForReady polls the /health endpoint until ready:true or the process
// dies, with a hard deadline of readyTimeout.
func waitForReady(pid int, lpath string) {
	cfg, _ := LoadConfig("")
	addr := ":8080"
	if cfg != nil {
		addr = cfg.ListenAddr
	}
	hURL := healthURL(addr)

	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(readyTimeout)
	fmt.Print("waiting for ready")
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			fmt.Println()
			fmt.Fprintf(os.Stderr, "ccg process died. Check %s\n", lpath)
			return
		}
		resp, err := client.Get(hURL)
		if err == nil {
			var result map[string]interface{}
			_ = json.NewDecoder(resp.Body).Decode(&result)
			resp.Body.Close()
			if ready, ok := result["ready"].(bool); ok && ready {
				fmt.Println()
				fmt.Println("ccg is ready")
				return
			}
		}
		fmt.Print(".")
		time.Sleep(readyPollInterval)
	}
	fmt.Println()
	fmt.Printf("warning: timed out after %s; gateway is running but may not be warm\n", readyTimeout)
}

// cmdStart starts the gateway as a background daemon. If FEISHU_APP_ID is
// not configured, the Feishu registration wizard runs interactively first.
func cmdStart(args []string) {
	pidPath := daemonPIDPath()
	if pid := readPID(pidPath); pid > 0 {
		log.Fatalf("ccg already running (PID %d). Use 'ccg restart' to reload.", pid)
	}

	// Auto-register Feishu app when no credentials are configured.
	cfg, _ := LoadConfig("")
	if cfg != nil && cfg.Feishu.AppID == "" {
		if !isatty() {
			log.Fatalf("No Feishu app configured and stdin is not a TTY.\n" +
				"  Run 'ccg register' interactively first, then retry 'ccg start'.")
		}
		fmt.Println("No Feishu app configured. Running registration wizard first...")
		fmt.Println()
		cmdRegister()
		fmt.Println()
	}

	lpath := daemonLogPath()
	if err := os.MkdirAll(filepath.Dir(lpath), 0755); err != nil {
		log.Fatalf("create log dir: %v", err)
	}
	logFile, err := os.OpenFile(lpath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("open log file %s: %v", lpath, err)
	}

	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("get executable path: %v", err)
	}

	cmdArgs := append([]string{"serve"}, args...)
	daemonCmd := exec.Command(exe, cmdArgs...)
	daemonCmd.Stdout = logFile
	daemonCmd.Stderr = logFile
	daemonCmd.Env = os.Environ()
	daemonCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := daemonCmd.Start(); err != nil {
		logFile.Close()
		log.Fatalf("start daemon: %v", err)
	}
	pid := daemonCmd.Process.Pid
	_ = daemonCmd.Process.Release()
	logFile.Close()

	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write PID file: %v\n", err)
	}

	fmt.Printf("ccg started (PID %d)\n", pid)
	fmt.Printf("  log: %s\n", lpath)
	waitForReady(pid, lpath)
}

// cmdStop sends SIGTERM to the daemon (SIGKILL after grace period).
func cmdStop() {
	pidPath := daemonPIDPath()
	pid := readPID(pidPath)
	if pid == 0 {
		fmt.Println("ccg is not running")
		return
	}
	if guardSelfKill(pid) {
		os.Exit(2)
	}

	fmt.Printf("stopping gateway (PID %d)...\n", pid)
	_ = syscall.Kill(pid, syscall.SIGTERM)

	deadline := time.Now().Add(stopGraceTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		if syscall.Kill(pid, 0) != nil {
			_ = os.Remove(pidPath)
			fmt.Println("ccg stopped")
			return
		}
	}

	fmt.Println("force killing...")
	_ = syscall.Kill(pid, syscall.SIGKILL)
	_ = os.Remove(pidPath)
	fmt.Println("ccg stopped")
}

// cmdRestart stops then starts the daemon.
func cmdRestart(args []string) {
	cmdStop()
	time.Sleep(500 * time.Millisecond)
	cmdStart(args)
}

// cmdStatus prints daemon status and health check result.
func cmdStatus() {
	pidPath := daemonPIDPath()
	pid := readPID(pidPath)
	if pid == 0 {
		fmt.Println("ccg is not running")
		return
	}
	fmt.Printf("ccg is running (PID %d)\n", pid)

	cfg, _ := LoadConfig("")
	addr := ":8080"
	if cfg != nil {
		addr = cfg.ListenAddr
	}
	hURL := healthURL(addr)

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(hURL)
	if err != nil {
		fmt.Printf("health: unreachable (%v)\n", err)
		return
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&result)
	out, _ := json.MarshalIndent(result, "  ", "  ")
	fmt.Printf("health: %s\n", out)
}

// cmdLogs tails the daemon log file.
// Flags: -n N (lines, default 100), --no-follow (disable -f).
func cmdLogs(args []string) {
	lpath := daemonLogPath()

	n := "100"
	follow := true
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-n":
			if i+1 < len(args) {
				n = args[i+1]
				i++
			}
		case "--no-follow", "-F":
			follow = false
		}
	}

	tailArgs := []string{"-n", n}
	if follow {
		tailArgs = append(tailArgs, "-f")
	}
	tailArgs = append(tailArgs, lpath)

	tailCmd := exec.Command("tail", tailArgs...)
	tailCmd.Stdout = os.Stdout
	tailCmd.Stderr = os.Stderr
	tailCmd.Stdin = os.Stdin
	if err := tailCmd.Run(); err != nil {
		log.Fatalf("tail: %v", err)
	}
}

// isatty reports whether stdin is an interactive terminal.
func isatty() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}
