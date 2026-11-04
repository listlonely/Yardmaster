// 桌面气泡通知（M9-W4，M6 砍项回收）：beeep 封装——Windows 10/11 走
// 原生 WinRT COM（jackmordaunt/go-toast，go-ole 纯 Go）+ PowerShell 兜底，
// Linux 走 godbus notify（依赖已在树），W1 探针双轮实证（方案定稿见
// 开发文档 §9.10 W1 锚点）。失败只降级（debug 留痕）不阻断。
//
// 同一 key（如上游账号）10 分钟限频——上游休眠事件会随每次请求重复
// 触发，气泡刷屏反而淹没关键信息；派发终态按会话 ID 天然不重复。
package app

import (
	_ "embed"
	"log/slog"
	"sync"
	"time"

	"github.com/gen2brain/beeep"
)

// notifyIconPNG 是桌面通知图标（tools/genwinres 从 appicon.png 生成的
// 64px 圆角透明 PNG；beeep Windows 端 []byte 分支按 PNG 解码——注意
// 与托盘的 ICO 是两份产物，喂错格式会静默失败）。
//
//go:embed notify_icon.png
var notifyIconPNG []byte

// restNotifyTTL 是同一 key 的通知限频窗口。
const restNotifyTTL = 10 * time.Minute

// notifier 是桌面气泡通知器（nil 安全：零值不发送）。
type notifier struct {
	enabled bool
	mu      sync.Mutex
	last    map[string]time.Time
}

func newNotifier(enabled bool) *notifier {
	return &notifier{enabled: enabled, last: map[string]time.Time{}}
}

// sendBeeep 是 beeep.Notify 的间接层（测试替换用）。
var sendBeeep = func(title, msg string) {
	if err := beeep.Notify(title, msg, notifyIconPNG); err != nil {
		slog.Debug("桌面通知失败（降级）", "title", title, "err", err)
	}
}

// send 弹出气泡；key 非空时限频（窗口内同 key 静默）。图标=appicon
// 同款（notify_icon.png，与托盘 ICO 同源不同格式）。
func (n *notifier) send(key, title, msg string) {
	if n == nil || !n.enabled {
		return
	}
	n.mu.Lock()
	if key != "" {
		if t, ok := n.last[key]; ok && time.Since(t) < restNotifyTTL {
			n.mu.Unlock()
			return
		}
		n.last[key] = time.Now()
	}
	n.mu.Unlock()
	sendBeeep(title, msg)
}
