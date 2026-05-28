package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mdp/qrterminal/v3"
)

const (
	feishuRegEndpoint = "https://accounts.feishu.cn/oauth/v1/app/registration"
	larkRegEndpoint   = "https://accounts.larksuite.com/oauth/v1/app/registration"
)

type regBeginResp struct {
	DeviceCode      string `json:"device_code"`
	VerifyURI       string `json:"verification_uri_complete"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	Error           string `json:"error"`
	ErrorDesc       string `json:"error_description"`
}

type regPollResp struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	UserInfo     struct {
		OpenID      string `json:"open_id"`
		TenantBrand string `json:"tenant_brand"`
	} `json:"user_info"`
	Error     string `json:"error"`
	ErrorDesc string `json:"error_description"`
}

// cmdRegister runs the interactive Feishu app registration wizard using
// OAuth 2.0 Device Authorization Grant. On success it writes FEISHU_APP_ID
// and FEISHU_APP_SECRET to ~/.ccg/.env.
func cmdRegister() {
	fmt.Println("飞书应用注册向导")
	fmt.Println("================")
	fmt.Println()

	beginResp, err := regBegin(feishuRegEndpoint)
	if err != nil {
		log.Fatalf("registration begin: %v", err)
	}

	qrURL := buildQRURL(beginResp.VerifyURI)

	fmt.Println("请用飞书 App 扫描以下二维码完成应用创建：")
	fmt.Println()
	qrterminal.Generate(qrURL, qrterminal.L, os.Stdout)

	expiresIn := beginResp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 600
	}
	fmt.Printf("\n二维码有效期：约 %d 分钟\n", expiresIn/60)
	fmt.Printf("也可以直接在浏览器打开：%s\n\n", qrURL)

	result, err := regPoll(beginResp.DeviceCode, beginResp.Interval, expiresIn)
	if err != nil {
		log.Fatalf("registration failed: %v", err)
	}

	tenant := result.UserInfo.TenantBrand
	if tenant == "" {
		tenant = "feishu"
	}

	fmt.Println("\n✓ 应用创建成功")
	fmt.Printf("  App ID:  %s\n", result.ClientID)
	fmt.Printf("  Tenant:  %s\n", tenant)
	if result.UserInfo.OpenID != "" {
		fmt.Printf("  用户:    %s\n", result.UserInfo.OpenID)
	}

	if err := saveFeishuEnv(result.ClientID, result.ClientSecret, result.UserInfo.OpenID); err != nil {
		log.Fatalf("save credentials: %v", err)
	}
}

func buildQRURL(verifyURI string) string {
	if strings.Contains(verifyURI, "?") {
		return verifyURI + "&from=sdk&source=ccg&tp=sdk"
	}
	return verifyURI + "?from=sdk&source=ccg&tp=sdk"
}

func regPost(endpoint string, params url.Values) ([]byte, error) {
	resp, err := http.PostForm(endpoint, params)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func regBegin(endpoint string) (*regBeginResp, error) {
	body, err := regPost(endpoint, url.Values{
		"action":            {"begin"},
		"archetype":         {"PersonalAgent"},
		"auth_method":       {"client_secret"},
		"request_user_info": {"open_id"},
	})
	if err != nil {
		return nil, err
	}
	var r regBeginResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse response: %v (body: %s)", err, body)
	}
	if r.Error != "" {
		return nil, fmt.Errorf("%s: %s", r.Error, r.ErrorDesc)
	}
	return &r, nil
}

// regPoll polls the registration endpoint until credentials are returned,
// the token expires, or the user denies.
func regPoll(deviceCode string, intervalSec, expiresInSec int) (*regPollResp, error) {
	interval := time.Duration(intervalSec) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(expiresInSec) * time.Second)
	endpoint := feishuRegEndpoint
	domainSwitched := false

	for time.Now().Before(deadline) {
		time.Sleep(interval)

		body, err := regPost(endpoint, url.Values{
			"action":      {"poll"},
			"device_code": {deviceCode},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "poll error: %v\n", err)
			continue
		}

		var r regPollResp
		if err := json.Unmarshal(body, &r); err != nil {
			fmt.Fprintf(os.Stderr, "poll parse error: %v\n", err)
			continue
		}

		// Lark (international) tenant: switch endpoint and re-poll immediately.
		if r.UserInfo.TenantBrand == "lark" && !domainSwitched && r.ClientID == "" {
			endpoint = larkRegEndpoint
			domainSwitched = true
			fmt.Println("识别到国际版租户，已切换到 larksuite.com 域名。")
			interval = 0 // poll immediately after switch
			continue
		}

		if r.ClientID != "" && r.ClientSecret != "" {
			return &r, nil
		}

		switch r.Error {
		case "authorization_pending":
			// normal, keep polling
		case "slow_down":
			interval += 5 * time.Second
			fmt.Println("轮询速度过快，已自动降速。")
		case "access_denied":
			return nil, fmt.Errorf("access denied: %s", r.ErrorDesc)
		case "expired_token":
			return nil, fmt.Errorf("二维码已过期，请重新运行 ccg register")
		case "":
			// no error, no credentials yet — keep waiting
		default:
			return nil, fmt.Errorf("registration error: %s - %s", r.Error, r.ErrorDesc)
		}
	}
	return nil, fmt.Errorf("注册超时，二维码已过期。请重新运行 ccg register")
}

// saveFeishuEnv upserts FEISHU_APP_ID, FEISHU_APP_SECRET, and (on first
// registration) FEISHU_ALLOWED_USER_IDS into ~/.ccg/.env, preserving all
// other existing keys. openID is the scanning user's open_id; when non-empty
// it is written as the initial allowed-user so the bot isn't open to everyone.
func saveFeishuEnv(appID, appSecret, openID string) error {
	envPath := filepath.Join(ccgDir(), ".env")

	existing := ""
	if data, err := os.ReadFile(envPath); err == nil {
		existing = string(data)
	}

	lines := strings.Split(existing, "\n")
	out := make([]string, 0, len(lines)+3)
	appIDSet, secretSet, allowedSet := false, false, false

	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "FEISHU_APP_ID="):
			out = append(out, "FEISHU_APP_ID="+appID)
			appIDSet = true
		case strings.HasPrefix(line, "FEISHU_APP_SECRET="):
			out = append(out, "FEISHU_APP_SECRET="+appSecret)
			secretSet = true
		case strings.HasPrefix(line, "FEISHU_ALLOWED_USER_IDS="):
			// Preserve existing allowed-user list; don't overwrite on re-register.
			out = append(out, line)
			allowedSet = true
		default:
			out = append(out, line)
		}
	}
	if !appIDSet {
		out = append(out, "FEISHU_APP_ID="+appID)
	}
	if !secretSet {
		out = append(out, "FEISHU_APP_SECRET="+appSecret)
	}
	if !allowedSet && openID != "" {
		out = append(out, "FEISHU_ALLOWED_USER_IDS="+openID)
	}

	content := strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
	if err := os.WriteFile(envPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("write %s: %v", envPath, err)
	}

	fmt.Printf("\n配置已保存到 %s\n", envPath)
	return nil
}
