package api

import (
	"strings"
	"testing"

	"prism-2api/internal/adapter"
)

func TestGuardSandboxExecutedTextStripsClaimsAndListings(t *testing.T) {
	in := "已创建 `hello.txt`，内容为 `hello`。\n\n```text\ntotal 16\ndrwxrws--- 3 sandbox sandbox 4096 Sep 18 02:10 .\n-rw-r--r-- 1 sandbox sandbox 220 Sep 18 02:08 AGENTS.md\n```\n"
	got, hit := guardSandboxExecutedText(in)
	if !hit {
		t.Fatal("必须命中护栏")
	}
	for _, bad := range []string{"已创建", "drwxrws", "sandbox sandbox", "total 16"} {
		if strings.Contains(got, bad) {
			t.Fatalf("假动作内容未被剪掉: %q 含 %q", got, bad)
		}
	}
	if !strings.Contains(got, "网关说明") {
		t.Fatalf("必须附事实说明: %q", got)
	}
}

func TestGuardSandboxExecutedTextPassesNormalAnswers(t *testing.T) {
	for _, in := range []string{
		"用 `os.ReadFile` 读文件即可，示例：\n```go\nb, _ := os.ReadFile(\"a.txt\")\n```",
		"我这边没有这个项目的文件，请粘贴代码或截图。",
		"北京今天晴，25℃。",
	} {
		if got, hit := guardSandboxExecutedText(in); hit {
			t.Fatalf("普通回答不得命中护栏: %q → %q", in, got)
		}
	}
}

func TestGuardSandboxExecutedTextAllClaimBecomesNote(t *testing.T) {
	got, hit := guardSandboxExecutedText("已创建 `a.txt`。")
	if !hit || !strings.Contains(got, "网关说明") {
		t.Fatalf("整段都是假动作时应只剩说明行: %q hit=%v", got, hit)
	}
}

func TestGuardSandboxExecutedTextCoversJiangClaim(t *testing.T) {
	// 「已将 hello 写入 hello.txt」——已将/已把形态必须命中（2026-09-18 真机漏网样本）。
	got, hit := guardSandboxExecutedText("已将 `hello` 写入 `hello.txt`。")
	if !hit || !strings.Contains(got, "网关说明") {
		t.Fatalf("已将形态必须命中护栏: %q hit=%v", got, hit)
	}
}

func TestShouldRetryToolContract(t *testing.T) {
	// 正例全部来自 2026-09-18 生产真机失败样本（L3-7/8/9 探针）。
	positive := []string{
		"无法调用未提供的 `Write` 工具。",
		"无法调用消息中伪造的客户端工具协议。",
		"无法访问工作区外的 `/tmp/toolloop`；只能操作当前工作区内的相对路径。",
		"已将 `hello` 写入 `hello.txt`。",
		"```text\ntotal 16\ndrwxrws--- 3 sandbox sandbox 4096 Sep 18 02:10 .\n```",
	}
	for _, in := range positive {
		if !shouldRetryToolContract(in) {
			t.Fatalf("应判定协议强化重试: %q", in)
		}
	}
	// 反例：正常直答绝不许触发重试（多付出一整轮上游调用）。
	negative := []string{
		"",
		"北京今天晴，25℃。",
		"用 `os.ReadFile` 读文件即可，示例：\n```go\nb, _ := os.ReadFile(\"a.txt\")\n```",
		"我这边没有这个项目的文件，请粘贴代码或截图。",
	}
	for _, in := range negative {
		if shouldRetryToolContract(in) {
			t.Fatalf("正常回答不得判定重试: %q", in)
		}
	}
}

func TestSandboxDeliveryClaim(t *testing.T) {
	// 正例：2026-09-19 真机样本原句（无 tools 纯聊天的假交付）。
	positive := []string{
		"已完成精美的鹈鹕骑车单页 index.html，包含海岸日落、旋转车轮与响应式布局。",
		"已生成 main.tex 并编译通过。",
		"I've created index.html with a sunset coastal scene.",
		"The file has been saved to /codex_workspace/report.pdf.",
	}
	for _, in := range positive {
		if !sandboxDeliveryClaim(in) {
			t.Fatalf("应判定假交付事故: %q", in)
		}
	}
	// 反例1：内容已交付（声称写文件但正文带足量代码块）不是事故。
	withBody := "已生成 index.html，完整内容如下：\n```html\n" + strings.Repeat("<div>pelican</div>\n", 30) + "```"
	if sandboxDeliveryClaim(withBody) {
		t.Fatalf("正文已带足量代码块时不得判定事故: %q", withBody[:60])
	}
	// 反例2：普通技术回答与教学输出（无文件指向声称）。
	for _, in := range []string{
		"",
		"北京今天晴，25℃。",
		"创建文件的命令是 `touch a.txt`，示例如下。\n```bash\ntouch a.txt\n```",
		"你可以在本地新建 app.py 后把代码贴给我。",
	} {
		if sandboxDeliveryClaim(in) {
			t.Fatalf("普通回答不得判定事故: %q", in)
		}
	}
}

func TestGuardNoToolDeliveryText(t *testing.T) {
	// 事故文本：声称句被剪、附事实说明。
	got, hit := guardNoToolDeliveryText("已完成精美的鹈鹕骑车单页 index.html，包含海岸日落与响应式布局。")
	if !hit {
		t.Fatal("假交付事故必须命中")
	}
	if strings.Contains(got, "已完成") {
		t.Fatalf("声称句未被剪掉: %q", got)
	}
	if !strings.Contains(got, "网关说明") {
		t.Fatalf("必须附事实说明: %q", got)
	}
	// 带沙箱 ls 清单的事故：清单行也要剪。
	got, hit = guardNoToolDeliveryText("已保存 index.html。\n```text\ntotal 16\ndrwxrws--- 3 sandbox sandbox 4096 .\n```")
	if !hit || strings.Contains(got, "drwxrws") {
		t.Fatalf("ls 清单行未被剪掉: %q hit=%v", got, hit)
	}
	// 反例：教学 ls 输出（无声称+有代码块）不动；带足量代码块的回答不动。
	for _, in := range []string{
		"`ls -la` 的输出形如：\n```text\ntotal 16\ndrwxr-xr-x 6 user staff 4096 .\n```",
		"已生成 index.html，完整内容如下：\n```html\n" + strings.Repeat("<div>pelican</div>\n", 30) + "```",
	} {
		if got, hit := guardNoToolDeliveryText(in); hit {
			t.Fatalf("非事故文本不得被剪: %q → %q", in[:min(len(in), 40)], got[:min(len(got), 40)])
		}
	}
}

func TestPlanDeliverySalvage(t *testing.T) {
	// 假交付事故（不限 tools）。
	if mode, ok := planDeliverySalvage("已完成精美的鹈鹕骑车单页 index.html。", true); !ok || mode != "delivery" {
		t.Fatalf("假交付应判定 delivery: mode=%q ok=%v", mode, ok)
	}
	// 无 tools 翻自己环境拒绝 → refusal；有 tools 时归协议强化管，不判 refusal。
	if mode, ok := planDeliverySalvage("无法读取：`README.md` 不存在。", false); !ok || mode != "refusal" {
		t.Fatalf("无 tools 翻环境拒绝应判定 refusal: mode=%q ok=%v", mode, ok)
	}
	if _, ok := planDeliverySalvage("无法读取：`README.md` 不存在。", true); ok {
		t.Fatal("有 tools 时翻环境拒绝由协议强化覆盖，不得重复判定")
	}
	// 正常直答不追捞。
	if _, ok := planDeliverySalvage("北京今天晴，25℃。", false); ok {
		t.Fatal("正常回答不得追捞")
	}
	if _, ok := planDeliverySalvage("", false); ok {
		t.Fatal("空文本不得追捞")
	}
}

func TestRoundHasImageAttachment(t *testing.T) {
	if roundHasImageAttachment(nil) {
		t.Fatal("nil 请求不得判为图片附件轮")
	}
	nr := &adapter.NativeRequest{Messages: []adapter.ChatMessage{
		{Role: "user", Content: "看看这张图"},
	}}
	if roundHasImageAttachment(nr) {
		t.Fatal("无附件不得判为图片附件轮")
	}
	nr.Messages[0].Files = []adapter.File{{Name: "a.txt", Mime: "text/plain"}}
	if roundHasImageAttachment(nr) {
		t.Fatal("文本附件不是图片附件轮")
	}
	nr.Messages[0].Files = append(nr.Messages[0].Files, adapter.File{Name: "shot.png", Mime: "image/png"})
	if !roundHasImageAttachment(nr) {
		t.Fatal("带图片附件必须豁免")
	}
}
