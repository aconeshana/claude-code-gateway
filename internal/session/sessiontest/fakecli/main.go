// Fake claude-code CLI for testing. Behavior is controlled via environment
// variables so tests can drive it without recompiling.
//
//	FAKE_CLI_SESSION_ID         override the session_id reported in the init msg
//	FAKE_CLI_INIT_DELAY_MS      delay (ms) before sending the init message
//	FAKE_CLI_NO_INIT            if "1", do not send the init message
//	FAKE_CLI_EXIT_AFTER_USER    if "1", exit cleanly after first result
//	FAKE_CLI_EXIT_CODE          when exiting after user, use this exit code
//	FAKE_CLI_FAIL_START         if "1", exit immediately with code 1
//	FAKE_CLI_RESULT_DELAY_MS    delay (ms) before sending the result
//	FAKE_CLI_RESULT_ERROR       if "1", result is_error=true
//	FAKE_CLI_PERMISSION_TOOL    on user, emit a control_request for this tool
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

func main() {
	if os.Getenv("FAKE_CLI_FAIL_START") == "1" {
		os.Exit(1)
	}

	sessionID := envOr("FAKE_CLI_SESSION_ID", "fake-cli-session")
	writer := bufio.NewWriter(os.Stdout)
	defer writer.Flush()

	if delayMs := envInt("FAKE_CLI_INIT_DELAY_MS"); delayMs > 0 {
		time.Sleep(time.Duration(delayMs) * time.Millisecond)
	}

	if os.Getenv("FAKE_CLI_NO_INIT") != "1" {
		writeJSON(writer, map[string]any{
			"type":           "system",
			"subtype":        "init",
			"session_id":     sessionID,
			"model":          "fake-model",
			"cwd":            mustWD(),
			"tools":          []string{},
			"permissionMode": "default",
			"uuid":           "init-uuid",
		})
	}

	turn := 0
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		var msg map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		msgType, _ := msg["type"].(string)
		switch msgType {
		case "user":
			turn++
			tool := os.Getenv("FAKE_CLI_PERMISSION_TOOL")
			if tool != "" {
				reqID := fmt.Sprintf("req-%d", turn)
				inner, _ := json.Marshal(map[string]any{
					"subtype":     "can_use_tool",
					"tool_name":   tool,
					"tool_use_id": fmt.Sprintf("tu-%d", turn),
				})
				writeJSON(writer, map[string]any{
					"type":       "control_request",
					"request_id": reqID,
					"request":    json.RawMessage(inner),
				})
				continue
			}
			if delay := envInt("FAKE_CLI_RESULT_DELAY_MS"); delay > 0 {
				time.Sleep(time.Duration(delay) * time.Millisecond)
			}
			writeJSON(writer, map[string]any{
				"type":           "result",
				"subtype":        "success",
				"duration_ms":    100,
				"is_error":       os.Getenv("FAKE_CLI_RESULT_ERROR") == "1",
				"num_turns":      turn,
				"result":         fmt.Sprintf("ok-%d", turn),
				"total_cost_usd": 0.0,
				"session_id":     sessionID,
				"uuid":           fmt.Sprintf("res-%d", turn),
			})
			if os.Getenv("FAKE_CLI_EXIT_AFTER_USER") == "1" {
				os.Exit(envIntDefault("FAKE_CLI_EXIT_CODE", 0))
			}
		case "control_request":
			req, _ := msg["request"].(map[string]any)
			subtype, _ := req["subtype"].(string)
			if subtype == "end_session" {
				return
			}
		case "control_response":
			// permission approved/denied: emit result
			turn++
			writeJSON(writer, map[string]any{
				"type":           "result",
				"subtype":        "success",
				"duration_ms":    100,
				"is_error":       false,
				"num_turns":      turn,
				"result":         "ok-after-perm",
				"total_cost_usd": 0.0,
				"session_id":     sessionID,
				"uuid":           fmt.Sprintf("res-%d", turn),
			})
		case "keep_alive":
			// silently accept
		}
	}
}

func writeJSON(w *bufio.Writer, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	w.Write(data)
	w.WriteByte('\n')
	w.Flush()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string) int {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	n, _ := strconv.Atoi(v)
	return n
}

func envIntDefault(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func mustWD() string {
	wd, _ := os.Getwd()
	return wd
}
