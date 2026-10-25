package admin

// 本文件是凭证文件的两条唯一入口：
//   - saveAuthFile：落盘（添加账号、粘贴回调登录）；
//   - safeAuthFileForDelete：删除前的全部校验。
//
// 「添加 API Key 账号」与「粘贴 localhost 回调完成登录」两条路径都要把一份凭证
// 交给宿主，只有走这里才不会各自长出一套「宿主写 + 兜底写盘」的副本。删除同理：
// 路径归一化在 hostbridge 边界完成，这里只负责「这个路径能不能删」。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

// authFileDirs 是宿主不可用时的兜底落盘目录：容器里是 /root/.cli-proxy-api，
// 本地直跑时是进程工作目录。
var authFileDirs = []string{"/root/.cli-proxy-api", "."}

// saveAuthFile 落盘一份凭证。
//
// 首选 host.auth.save：它写 auths 目录并热更新宿主内存里的注册表。只有这条
// 回调不可用时才直接写盘（与凭证续期写盘兜底同一个理由）。两条都失败才算失败——
// 调用方必须能把「没保存成功」如实告诉用户，不能出现页面显示成功而磁盘上什么都没有。
func saveAuthFile(callbackID, fileName string, raw []byte) error {
	name := strings.TrimSpace(fileName)
	if name == "" || len(raw) == 0 {
		return fmt.Errorf("凭证缺少文件名或内容")
	}

	// hostbridge.SaveAuth 同时负责 uid 大小写归一：落盘兜底必须写同一份内容，
	// 否则磁盘上的 uid 与宿主登记键不一致，Windows 上会多出一条重复账号。
	aligned, errSave := hostbridge.SaveAuth(callbackID, name, raw)
	if errSave == nil {
		logger.Info("saved auth file %s via host.auth.save", name)
		return nil
	}
	logger.Debug("host.auth.save failed for %s: %v", name, errSave)

	for _, dir := range authFileDirs {
		if fi, errStat := os.Stat(dir); errStat != nil || !fi.IsDir() {
			continue
		}
		if errWrite := os.WriteFile(filepath.Join(dir, name), aligned, 0o600); errWrite == nil {
			logger.Info("saved auth file %s on disk", name)
			return nil
		}
	}
	return fmt.Errorf("host.auth.save 不可用且落盘失败（%s）", name)
}

// safeAuthFileForDelete 校验一个凭证文件是否可以被删除，返回要删的绝对路径。
//
// 入参 path 必须已经是 hostbridge 归一化过的路径（ListAuths / AuthJSONAndPath
// 的出口都会做这件事）；这里不再换算，只校验——同一件事在两个层各做一次，
// 迟早会出现「有的调用点忘记归一化」的分歧。
//
// 四道检查的顺序是从「不值得尝试」到「必须实际落盘核对」：
//  1. 宿主给的凭证必须属于本插件；运行时凭证没有文件（RuntimeOnly / 路径为空）
//     → 直接拒绝；
//  2. 路径必须是绝对的；文件必须存在——**不存在不能当成功**：静默返回 nil 会让
//     页面显示「已删除」而账号其实还在；
//  3. 只删普通 `.json` 文件：符号链接、目录、别的扩展名一律拒绝；
//  4. 目标文件里的凭证必须与宿主给的凭证是同一个账号（vendor + UID）——路径
//     归一化用的是进程工作目录，基准不一致时同名文件可能属于另一个账号。
//     只比对身份字段，不读取或记录 token。
func safeAuthFileForDelete(registry *core.Registry, entry core.HostAuth, raw []byte, path string) (string, error) {
	hostCred, okHost := registry.ParseCredential(raw, hostbridge.AccountIdentity(entry), nil)
	if !okHost {
		return "", fmt.Errorf("该凭证不属于本插件，拒绝删除")
	}
	target := strings.TrimSpace(path)
	if entry.RuntimeOnly || target == "" {
		return "", fmt.Errorf("该账号没有对应的凭证文件（运行时凭证），无法删除")
	}
	if !filepath.IsAbs(target) {
		return "", fmt.Errorf("凭证路径未经宿主边界归一化：%s", target)
	}
	if !strings.HasSuffix(strings.ToLower(target), ".json") {
		return "", fmt.Errorf("拒绝删除非凭证文件：%s", filepath.Base(target))
	}
	info, errStat := os.Lstat(target)
	if errStat != nil {
		if os.IsNotExist(errStat) {
			return "", fmt.Errorf("凭证文件不存在，可能已被宿主移除：%s", filepath.Base(target))
		}
		return "", fmt.Errorf("检查文件失败：%w", errStat)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("拒绝删除符号链接：%s", filepath.Base(target))
	}
	if info.IsDir() {
		return "", fmt.Errorf("拒绝删除目录：%s", filepath.Base(target))
	}

	onDisk, errRead := os.ReadFile(target)
	if errRead != nil {
		return "", fmt.Errorf("读取凭证文件失败：%w", errRead)
	}
	fileCred, okFile := registry.ParseCredential(onDisk, strings.TrimSuffix(filepath.Base(target), ".json"), nil)
	if !okFile || !sameCredentialIdentity(hostCred, fileCred) {
		return "", fmt.Errorf("目标文件属于另一个账号，拒绝删除：%s", filepath.Base(target))
	}
	return target, nil
}

// sameCredentialIdentity 判断两份凭证是否指向同一个账号。
//
// vendor 或 UID 取不到时跳过该维度（部分供应商凭证里没有 UID）：宁可放行，
// 也不要卡住正常删除。只比对身份字段，不涉及 token。
func sameCredentialIdentity(hostCred, fileCred *core.Credential) bool {
	if hostVendor, fileVendor := hostCred.VendorIDValue(), fileCred.VendorIDValue(); hostVendor != "" && fileVendor != "" && hostVendor != fileVendor {
		return false
	}
	hostUID, fileUID := hostCred.UIDValue(), fileCred.UIDValue()
	return hostUID == "" || fileUID == "" || hostUID == fileUID
}
