package qoder

// 本文件实现 Qoder 设备身份的本机自动解析。
//
// 上游对 cosy-machinetoken + cosy-machinetype 做精确校验：缺少这一对头时
// /sash/api/v1/me/campaigns 只下发 VIEW_DETAILS，**不会**下发可领取的
// CLAIM_BENEFIT，于是插件误判「今天没有可领取的活动」。
//
// 身份由官方组件 runtime-info 生成，且与 environment 参数绑定。这里按
// 参考实现（dsh-our-free-model）的口径：
//   1. 主路径：spawn 本机 Qoder 客户端的 runtime-info <env> --account-stdin；
//   2. 退路：读取客户端缓存的 machine_token.json；
//   3. 都拿不到时返回空身份（不发机器头），由用户手工配置兜底。
//
// 用户显式提供的身份（凭证字段 / 全局配置）优先于本机自动解析。

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	// runtimeInfoEnvironment 是 runtime-info 的第一个位置参数。
	// 3 = 国际版（官方 asar 里 environment === 'global' ? 3 : 0）。
	// 漏掉它会拿到另一套身份，服务端因此不下发可领取活动。
	runtimeInfoEnvironment = "3"
	// runtimeInfoTimeout 限制单次 spawn（实测正常约 3.8 秒）。
	runtimeInfoTimeout = 20 * time.Second
)

// qoderDataDirNames 是本机 Qoder 客户端的数据目录名（国际版 / 国内版）。
var qoderDataDirNames = []string{".qoder", ".qoder-cn"}

var (
	localIdentityOnce  sync.Once
	localIdentityValue MachineIdentity
)

// resolveLocalMachineIdentity 解析本机设备身份；结果在进程内缓存。
func resolveLocalMachineIdentity() MachineIdentity {
	localIdentityOnce.Do(func() {
		if identity, okIdentity := readRuntimeInfoIdentity(); okIdentity {
			localIdentityValue = identity
			return
		}
		for _, path := range machineTokenPaths() {
			if identity, okIdentity := readMachineTokenFile(path); okIdentity {
				localIdentityValue = identity
				return
			}
		}
	})
	return localIdentityValue
}

// resetLocalMachineIdentityCache 仅供测试清空缓存。
func resetLocalMachineIdentityCache() {
	localIdentityOnce = sync.Once{}
	localIdentityValue = MachineIdentity{}
}

// readRuntimeInfoIdentity 实时调用官方 runtime-info 生成身份。
func readRuntimeInfoIdentity() (MachineIdentity, bool) {
	executable, okExecutable := locateRuntimeInfo()
	if !okExecutable {
		return MachineIdentity{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), runtimeInfoTimeout)
	defer cancel()

	command := exec.CommandContext(ctx, executable, runtimeInfoEnvironment, "--account-stdin")
	command.Stdin = strings.NewReader(`{"account":""}`)
	output, errRun := command.Output()
	if errRun != nil {
		return MachineIdentity{}, false
	}
	return parseRuntimeInfoOutput(string(output))
}

// locateRuntimeInfo 定位本机 runtime-info 可执行文件。
//
// 路径形态：<home>/<dataDir>/.bin/umid-<hash>/runtime-info(.exe)。
// 目录名带哈希（随版本变化），因此枚举而不是写死。
func locateRuntimeInfo() (string, bool) {
	if override := strings.TrimSpace(os.Getenv("QODER_RUNTIME_INFO")); override != "" {
		if fileExists(override) {
			return override, true
		}
		return "", false
	}
	home, errHome := os.UserHomeDir()
	if errHome != nil {
		return "", false
	}
	name := "runtime-info"
	if runtime.GOOS == "windows" {
		name = "runtime-info.exe"
	}
	for _, dirName := range qoderDataDirNames {
		binDir := filepath.Join(home, dirName, ".bin")
		entries, errRead := os.ReadDir(binDir)
		if errRead != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "umid-") {
				continue
			}
			candidate := filepath.Join(binDir, entry.Name(), name)
			if fileExists(candidate) {
				return candidate, true
			}
		}
	}
	return "", false
}

// parseRuntimeInfoOutput 从 runtime-info 的 JSON 输出里取出身份。
//
// 输出可能带前缀日志，故从第一个 '{' 开始解析。
func parseRuntimeInfoOutput(output string) (MachineIdentity, bool) {
	start := strings.IndexByte(output, '{')
	if start < 0 {
		return MachineIdentity{}, false
	}
	var payload struct {
		MachineToken string `json:"machineToken"`
		MachineType  string `json:"machineType"`
		MachineCode  string `json:"machineCode"`
	}
	if errUnmarshal := json.Unmarshal([]byte(output[start:]), &payload); errUnmarshal != nil {
		return MachineIdentity{}, false
	}
	token := strings.TrimSpace(payload.MachineToken)
	machineType := strings.TrimSpace(payload.MachineType)
	if token == "" || machineType == "" {
		return MachineIdentity{}, false
	}
	return MachineIdentity{Token: token, Type: machineType, Code: strings.TrimSpace(payload.MachineCode)}, true
}

// machineTokenPaths 返回客户端磁盘缓存的候选路径（退路）。
func machineTokenPaths() []string {
	if override := strings.TrimSpace(os.Getenv("QODER_MACHINE_TOKEN_PATH")); override != "" {
		return []string{override}
	}
	paths := make([]string, 0, 6)
	if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
		for _, dirName := range []string{"Qoder", "Qoder CN"} {
			paths = append(paths, filepath.Join(appData, dirName, "SharedClientCache", "cache", "machine_token.json"))
		}
	}
	home, errHome := os.UserHomeDir()
	if errHome == nil {
		for _, dirName := range []string{"Qoder", "Qoder CN"} {
			paths = append(paths,
				filepath.Join(home, "Library", "Application Support", dirName, "SharedClientCache", "cache", "machine_token.json"),
				filepath.Join(home, ".config", dirName, "SharedClientCache", "cache", "machine_token.json"))
		}
	}
	return paths
}

// readMachineTokenFile 读取客户端缓存的身份文件。
func readMachineTokenFile(path string) (MachineIdentity, bool) {
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return MachineIdentity{}, false
	}
	var payload struct {
		Token string `json:"token"`
		Type  string `json:"type"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return MachineIdentity{}, false
	}
	token := strings.TrimSpace(payload.Token)
	machineType := strings.TrimSpace(payload.Type)
	if token == "" || machineType == "" {
		return MachineIdentity{}, false
	}
	return MachineIdentity{Token: token, Type: machineType}, true
}

func fileExists(path string) bool {
	info, errStat := os.Stat(path)
	return errStat == nil && !info.IsDir()
}
