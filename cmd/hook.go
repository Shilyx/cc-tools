package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"cctools/pkg/encoding"

	"github.com/spf13/cobra"
)

// legacyCharset 是"非 UTF-8 文件"统一按此编码处理的目标。
// 选择 GB18030 的理由(经用户确认):
//   1. 它是 GBK 的超集,可无损解码简体中文遗留文件;
//   2. 它是 Unicode 全覆盖编码——任何 UTF-8 内容都能编码回去,
//      所以会话期间 Claude 新增的任意字符在 restore 时都不会编码失败;
//   3. 避免 chardet 在"短文本/少 CJK"文件上把 GBK 误判为 ISO-8859-1 的问题
//      (实测 `// 原始注释\nint a=1;` 这类短文件会被误判,导致解码/还原错误)。
const legacyCharset = "GB18030"

// ===========================================================================
// hook: Claude Code 编码守卫
//
//   cctools hook pre       挂 PreToolUse(Read|Edit|Write)。命中 C++ 文件且非
//                          UTF-8 时,把磁盘文件转成 UTF-8,并把原编码登记到会话
//                          级登记表。Claude 全程读到/编辑的都是 UTF-8,内置 Edit
//                          的精确字符串匹配不会因乱码而失败。
//
//   cctools hook restore   挂 Stop。会话结束时把所有被转过的文件转回原编码,清表。
//
// 复用 cc-tools 已验证的 pkg/encoding(chardet 检测 + golang.org/x/text 转换)。
// ===========================================================================

// C++ 源文件扩展名
var cppExts = map[string]bool{
	".c": true, ".cc": true, ".cpp": true, ".cxx": true, ".c++": true,
	".h": true, ".hh": true, ".hpp": true, ".hxx": true, ".h++": true,
	".inl": true, ".ipp": true, ".tcc": true,
}

type hookInput struct {
	SessionID string `json:"session_id"`
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		FilePath string `json:"file_path"`
	} `json:"tool_input"`
}

var msysPathRe = regexp.MustCompile(`^/([a-zA-Z])/(.*)$`)

// normalizePath 把 Git Bash 的 MSYS 路径 (/f/foo) 归一化为 Windows 形式 (F:/foo),
// 让原生 Windows 程序也能打开。非 MSYS 路径原样返回。
func normalizePath(p string) string {
	if m := msysPathRe.FindStringSubmatch(p); m != nil {
		return strings.ToUpper(m[1]) + ":/" + m[2]
	}
	return p
}

// canonicalCharset 已移除:检测策略改为 utf8.Valid → 否则 GB18030(见 legacyCharset),
// 不再依赖 chardet 的字符集命名(它在短文本上会把 GBK 误判为 ISO-8859-1)。

func isCppFile(p string) bool {
	return cppExts[strings.ToLower(filepath.Ext(p))]
}

// registryPath 返回会话级登记表路径(系统临时目录)。
func registryPath(sessionID string) string {
	if sessionID == "" {
		sessionID = "nosess"
	}
	safe := regexp.MustCompile(`[^A-Za-z0-9_-]`).ReplaceAllString(sessionID, "_")
	return filepath.Join(os.TempDir(), "cc-cpp-enc-"+safe+".tsv")
}

func loadRegistry(path string) map[string]string {
	m := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) == 2 {
			m[parts[0]] = parts[1]
		}
	}
	return m
}

func saveRegistry(path string, m map[string]string) error {
	var b strings.Builder
	for k, v := range m {
		b.WriteString(k)
		b.WriteByte('\t')
		b.WriteString(v)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0644)
}

func emitSystemMessage(msg string) {
	out, _ := json.Marshal(struct {
		SystemMessage string `json:"systemMessage"`
	}{msg})
	fmt.Println(string(out))
}

// readHookInput 从 stdin 读取 Claude Code 发来的 hook JSON(UTF-8)。
func readHookInput() (*hookInput, error) {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, err
	}
	var in hookInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	return &in, nil
}

// ---- cctools hook ----

var hookCmd = &cobra.Command{
	Use:   "hook",
	Short: "Claude Code 编码守卫钩子(pre / restore)",
	Long: `在 Claude Code 编辑 C++ 文件前后透明地处理非 UTF-8 编码。

  pre      PreToolUse(Read|Edit|Write):非 UTF-8 的 C++ 文件转成 UTF-8 并登记原编码
  restore  Stop:会话结束时把登记过的文件转回原编码

钩子永远以退出码 0 返回(尽力而为),绝不阻塞编辑。`,
}

var hookPreCmd = &cobra.Command{
	Use:   "pre",
	Short: "PreToolUse:非 UTF-8 的 C++ 文件转 UTF-8 并登记",
	RunE:  runHookPre,
}

var hookRestoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Stop:把登记过的文件转回原编码",
	RunE:  runHookRestore,
}

func init() {
	rootCmd.AddCommand(hookCmd)
	hookCmd.AddCommand(hookPreCmd)
	hookCmd.AddCommand(hookRestoreCmd)
}

func runHookPre(cmd *cobra.Command, args []string) error {
	in, err := readHookInput()
	if err != nil {
		return nil // 输入坏了也不阻塞
	}
	path := normalizePath(in.ToolInput.FilePath)
	if path == "" || !isCppFile(path) {
		return nil
	}
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		return nil // 不存在(如新建文件)或是目录,跳过
	}

	regPath := registryPath(in.SessionID)
	reg := loadRegistry(regPath)
	if _, done := reg[path]; done {
		return nil // 本会话已转过,磁盘上已是 UTF-8
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if utf8.Valid(raw) {
		return nil // 已是 UTF-8(纯 ASCII 也算),无需处理
	}

	// 非 UTF-8:按 GB18030 解码后重写为 UTF-8
	text, err := encoding.DecodeBytes(raw, legacyCharset)
	if err != nil {
		emitSystemMessage(fmt.Sprintf("[cpp-encoding] %s 非 UTF-8 且按 %s 解码失败,已跳过转码",
			filepath.Base(path), legacyCharset))
		return nil
	}
	if err := os.WriteFile(path, []byte(text), 0644); err != nil {
		return nil
	}

	reg[path] = legacyCharset
	_ = saveRegistry(regPath, reg)
	emitSystemMessage(fmt.Sprintf("[cpp-encoding] %s: %s → UTF-8(会话结束时自动转回)",
		filepath.Base(path), legacyCharset))
	return nil
}

func runHookRestore(cmd *cobra.Command, args []string) error {
	in, err := readHookInput()
	if err != nil {
		return nil
	}
	regPath := registryPath(in.SessionID)
	reg := loadRegistry(regPath)
	if len(reg) == 0 {
		return nil
	}

	var restored, failed []string
	for path, enc := range reg {
		if fi, statErr := os.Stat(path); statErr != nil || fi.IsDir() {
			continue
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			failed = append(failed, filepath.Base(path))
			continue
		}
		// 文件当前应为 UTF-8;按原编码重新编码写回
		encoded, encErr := encoding.EncodeString(string(raw), enc)
		if encErr != nil {
			failed = append(failed, filepath.Base(path))
			continue
		}
		if writeErr := os.WriteFile(path, encoded, 0644); writeErr != nil {
			failed = append(failed, filepath.Base(path))
			continue
		}
		restored = append(restored, filepath.Base(path))
	}

	_ = os.Remove(regPath)

	if len(restored) > 0 || len(failed) > 0 {
		var parts []string
		if len(restored) > 0 {
			parts = append(parts, "已转回原编码: "+strings.Join(restored, ", "))
		}
		if len(failed) > 0 {
			parts = append(parts, "转回失败(保持 UTF-8): "+strings.Join(failed, ", "))
		}
		emitSystemMessage("[cpp-encoding] " + strings.Join(parts, "; "))
	}
	return nil
}
