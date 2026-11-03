# app 子系统——桌面壳（M8-W1 设计稿）

> [2026-11-02-桌面壳可行性] M8-W1 出稿（开发文档 §9.9 主线可行性复核
> 点，M5-W3 / M7-W2 同款纪律：首日出稿评审，不可行切备选）。状态：
> **设计可行，主线采纳**。核心实证：Wails v3.0.0 已于 2026-10-26 发布
> 稳定版（非 alpha）；Windows 后端库内 syscall 实现零 cgo（`CGO_ENABLED=0`
> 构建通过 + 运行冒烟通过）；build tag 隔离下 Linux 交叉构建不受影响
> （wails 未链入）。依据见 §二，设计定稿见 §三~§六。2026-11-02 增补
> §七：Magpie 设计语言参考（用户定向；同日二次澄清——**布局本身采
> 纳**，侧栏信息架构升级为 W3 主项，见 §七·甲）。

> [2026-11-02-M8W2 骨架窗口交付] §九清单 1~5 落地：window_wails.go
> （唯一 wails import 点）+ app.go 挂点分派 + 依赖边界断言 + go.sum
> 锁定 v3.0.0 + 冒烟。实测与偏差：
> - **关窗拦截的实现与设计稿预想不同**：v3.0.0 Windows 的 WM_CLOSE
>   默认被吞并 emit `Windows.WindowClosing`（webview_window_windows.go
>   WM_CLOSE 分支 return 0），真正销毁走默认映射
>   `Windows.WindowClosing→Common.WindowClosing` 的内部订阅者——
>   接管方式=覆盖 `WindowsWindow.EventMapping` 删除该映射 + 自订阅
>   平台事件 → `win.Hide()`。
> - 冒烟实证：原生窗口起（标题 Yardmaster）、healthz 200、WM_CLOSE 后
>   **进程存活 + healthz 仍 200**（默认行为是「最后窗口关闭即 auto
>   quit」，接管生效）；Go-WebView2Loader=true、CGO_ENABLED=0。
> - 体积实测 13.7→21.1MB（wails 增量 ~7.4MB，低于 W1 探针预估 15MB
>   ——探针含更多 wails 子包）。
> - 生命周期收敛：Ctrl-C 与托盘退出统一经 ctx→`app.Quit()` 单路径；
>   原生壳启动后故障回退浏览器窗口且网关不陪葬（§五第二道闸，代码
>   路径落实）。
> - A/B 对照（git worktree v0.7.0）：`systray error: unable to set
>   icon: The operation completed successfully` 为**既有怪癖**（fyne
>   systray 的 Shell_NotifyIcon 假阴性），非 W2 引入，图标实际正常。
> - 依赖边界：TestCorePackagesNoWailsDependency（7 核心包 go list 零
>   wails）PASS + ci.yml ubuntu grep 步骤兜底。
> - 未实测（如实标注）：托盘菜单真机点击（「打开窗口」Show/Focus 重
>   开、「退出」收尾顺序）待用户日常确认；WM_CLOSE 隐藏后
>   Get-Process MainWindowHandle 指向进程另一顶层窗口（句柄探测假象），
>   窗口实际隐藏以日常观感为准。

> [2026-11-02-M8W3 窗口体验交付] §9.9 W3 全项落地。实测与偏差：
> - **侧栏信息架构落地**：`#tabs` 原样平移进常暗侧栏（data-tab/JS 切页
>   零改动），四分组（运行/路由/观测/自动化）+ 16px 内联 SVG 图标 + 侧栏
>   脚部主题按钮；瘦顶栏 page-title 随页切换（data-title）；内容区统一
>   通栏（浏览器态去 64rem 限宽）。Magpie 基线 v0.1.2699 无增量。
> - **双主题令牌层**：亮色沿用自家工业风值，暗色为深蓝黑自配色（非照抄
>   magpie 色值，仅借鉴 data-theme 覆盖模式与 Okabe–Ito 系列令牌）；
>   组件硬编码色全部令牌化（badge/pill/bar/modal/code/table/danger 等）；
>   `:root.win` 字体栈由 JS 按 UA 加类。Okabe–Ito --c1..--c7 令牌就位，
>   图表消费待后续（当前无图表组件）。
> - **偏差 1（主题存储）**：设计稿原写「settings 新键」，落地为
>   **localStorage**——主题是查看端设备偏好，局域网多客户端应各自偏好，
>   进网关配置语义错误。
> - **偏差 2（状态记忆存储）**：设计稿原写「settings.json 新键」，落地为
>   **数据目录 window-state.json 独立文件**——壳层状态不进配置写闸
>   （五铁律仪式过度），读不懂=缺省 fail-soft；保存时机=关窗隐藏前+退出
>   前（移动后强杀不保存，如实留档）。
> - **偏差 3（外链拦截降级）**：wails v3.0.0 无 NewWindowRequested 类
>   API，「系统浏览器跳转」需 JS 桥（非目标）——落地为**前端拦截+toast
>   提示**；控制台当前外链面为零，属防御性兜底。
> - 浏览器实测三轮全绿：亮色布局 4/4（侧栏 212px 常暗/分组图标/瘦顶栏/
>   统计卡渲染）、主题三态 3/3（循环+暗色令牌+回归 auto）、app 态六页
>   遍历 7/7（page-title 联动/active/渲染/零 JS 错误）。
> - 真机冒烟：原生窗口起+healthz 200；**WM_CLOSE → window-state.json
>   落盘**（{"x":360,"y":106,"w":1200,"h":820,"max":false}）+ 关窗隐藏
>   进程存活。窗口背景色跟系统深浅色（AppsUseLightTheme 注册表探测）。
> - 未实测：窗口最大化/恢复路径（state.Max 恢复逻辑有代码无真机演练，
>   日常使用覆盖）、主题切换的 WebView2 背景色运行时联动（仅初始值，
>   运行中切换由页面自身背景承接）。

> [2026-11-03-M8W5 打磨分发交付] §9.9 W5 全项落地。实测与偏差：
> - **exe 资源嵌入**：tools/genwinres（tc-hib/winres v0.3.1 纯 Go，wails
>   同源依赖）预生成 `cmd/yardmaster/rsrc_windows_amd64.syso` 入库——
>   图标（trayIcon 同款矢量按 32 网格放大，256/64/48/32/16）、版本信息
>   （v0.8.0，中文 FileDescription UTF-16 正常）、per-monitor-v2 DPI
>   manifest（进程启动即 aware，wails 运行时检测另有兜底）。`go generate
>   ./cmd/yardmaster` 再生成；发版更新 genwinres 里 appVersion 常量。
>   实证：exe 属性 ProductName/FileVersion/中文描述/MIT 齐全、图标
>   ExtractAssociatedIcon 32x32、Linux 交叉构建按文件名忽略 syso。
> - **壳层 panic 隔离（语义同插件纪律）**：托盘 goroutine
>   runTrayIsolated（recover 后网关继续运行，失去托盘入口 Ctrl-C 退出，
>   stderr 留痕）；wails 主循环 panic 转 error 走既有浏览器回退路径。
>   recover 路径无法注入真实 panic——代码审查+正常路径冒烟覆盖，如实
>   标注。
> - **install/package 复核零改动**：package.sh 打包三平台二进制+README
>   （.syso 已链接进二进制，打包层无感知）；install.ps1 下载解压加
>   PATH——与壳层改动零耦合；产物清单形态不变（2 tar.gz + 1 zip +
>   SHA256SUMS + 2 install 脚本）。
> - 资源版本（0.8.0）与运行时版本（-ldflags 注入）互不依赖，如实
>   记录。TUI/CLI 对齐小项按可砍清单砍除。

> [2026-11-03-M8W6 冒烟发布交付] §9.9 W6 全项落地，v0.8.0 发布。冒烟
> 记录：①Windows 真机全链路——无参数启动原生窗口（标题 Yardmaster）+
> healthz 200 + **控制台写路径闭环**（settings 同值回写经完整写路径，
> effect=restart + 写响应含 config）+ 关窗语义（WM_CLOSE → 隐藏到托盘，
> 进程存活）——S1~S4 全绿；②**WebView2 缺失回退演练落地**：新增应急
> 开关 `YARDMASTER_NO_NATIVE_WINDOW=1`（W1 设计的注册表改键方案需管理
> 员权限且不可逆风险，环境变量钩子等价且顺带成为用户应急出口）——
> 置位后 stdout 留痕「原生窗口不可用（…置位），回退浏览器窗口」+
> **msedge --app=http://127.0.0.1:8317 窗口真被拉起** + 网关 200；③
> Linux 构建验证（W4 三目标交叉 + 零 wails 断言）与 race 全绿（CI 双
> 平台）；④托盘菜单真机点击仍留档待日常确认。§十未实测项相应更新。

> [2026-11-03-M9W4 托盘通知与菜单动态化] M6 砍项回收落地：①**桌面
> 气泡通知**（internal/app/notify.go）——beeep v0.11.3（W1 探针定稿
> 方案：Win10/11 原生 WinRT COM + PowerShell 兜底，Linux godbus），
> 图标=trayIcon 同款字节；事件两类：派发终态（`Engine.OnFinish` 链式
> 接在派发 trace 观察者后，语义不破坏）+ 上游账号休眠（`Server.
> OnAccountRest` 钩子：429 精准/默认、402、余额轮询耗尽三处；网络失败
> 不回调）；同 key 10 分钟限频（休眠事件随请求重复触发，防刷屏）；
> settings `notify` 指针语义（nil=缺省开，显式 false 关；cfgSettings
> nil 保留现值与 otel 同规）；失败 debug 留痕降级不阻断。真机冒烟：
> smoke 派发 done → 「派发完成 · smoke · 任务摘要」气泡弹出（用户确认）。
> ②**托盘菜单动态化**（Magpie traymenu 同构）：版本行（禁用态；dev 态
> 显示「dev（开发态）」避免 vdev）+「升级到 vX.Y.Z」（15s 首查 + 每
> 1h，有新版才显示，点击开 releases 页；查询失败静默；dev 态跳过）+
> 状态摘要行（休眠 N · 派发 M 进行中，30s 刷新；禁用态）。TrayOptions
> 取代固定双回调签名，app/serve -tray 两模式共用 wireDesktopHooks。
> beeep 依赖仅 internal/app 壳层引入（网关核心零感知，OnAccountRest
> 弱钩子解耦）。

> [2026-11-03-M8W6 后补丁：双击启动黑窗收起] 用户反馈「每次启动应用会
> 打开终端」——console subsystem 的 exe 双击/开始菜单启动时 conhost 黑
> 窗全程伴随（app 模式常驻=黑窗常驻）。方案：**保留 console subsystem**
> （serve/CLI 终端体验零变化），app 模式检测孤立启动（父进程=
> explorer.exe，Toolhelp32 纯 syscall）→ `FreeConsole` 收起黑窗 +
> slog 落数据目录 `app.log`（>1MB 轮转 .1）保住崩溃/回退/托盘隔离留痕
> ——此前这些留痕在双击场景随黑窗消失，属可观测性补强。终端启动零
> 变化。实证：explorer 代理启动无 conhost 子进程 + app.log 生成 +
> healthz 200（S1~S5）；Start-Process 启动 banner 正常 + app.log 不
> 生成（非孤立不接管，S1~S3）。文件：console_windows.go /
> console_other.go（!windows no-op）+ runApp 开头接线。

> [2026-11-03-黑窗根治：GUI 桌面双入口] 用户二轮反馈「启动时还会弹出
> 终端再关闭」——FreeConsole 方案的固有局限：conhost 在进程创建瞬间
> 弹出，收起只能事后（闪现 1~2s 不可消除）。根治=桌面入口换 **Windows
> GUI subsystem**：新增 `cmd/yardmaster-app`（与 cmd/yardmaster 共享
> internal/app 全部逻辑，仅链接期 `-H=windowsgui` + `guiSubsystem=1`
> 注入不同）——双击/开始菜单**零终端窗口零闪现**（GUI 进程不创建
> console，机制性）；console 版保留（serve/CLI 不变、`dispatch run`/
> `run` 的 headless 同步等待+退出码契约为硬功能，GUI subsystem 无法
> 满足，故不分体替换）。资源：genwinres 输出两份 syso（Go 只链接 main
> 包目录下的 syso——初版 app exe 属性为空即此坑）；GUI 版无控制台，
> `guiSubsystem=1` 恒走 app.log。发布链：release.yml 增 GUI 构建、
> package.sh 并入 windows zip（产物文件数不变）。实证：explorer 代理
> 启动零 conhost + app.log + healthz 200 + exe 属性/图标齐全（S1~S5）。
> 单实例/数据目录/配置语义两入口完全一致。

## 一、目标与边界

兑现 2026-10-30 桌面壳两步走决策第二步：Windows app 模式以 Wails v3
真原生窗口（WebView2）替代浏览器 `--app` 独立窗口；Linux 维持现有零
依赖浏览器窗口方案。

铁律边界（延续不变）：

- **webview 依赖禁止进入网关核心**——wails 仅落 `internal/app` 壳层，
  build tag 隔离（§三）。
- **交叉编译矩阵不变**：Linux amd64/arm64 + Windows amd64，Linux 侧纯
  Go 零 cgo（wails Linux 后端依赖 GTK4+WebKitGTK6 系统库 cgo，非目标）。
- `serve` / `console` 子命令行为零变化；wails 仅在 `runApp` 路径。
- 配置写路径五铁律（窗口内控制台同走管理 API，无新写路径）。

## 二、可行性核实记录（2026-11-02 实测）

| # | 核实点 | 结论 | 证据 |
| :-- | :-- | :-- | :-- |
| 1 | 版本状态 | **稳定版可用** | Wails v3.0.0 发布于 2026-10-26（blog）；官方 FAQ/状态页（2026-10-29 更新）：v3.0.0 为 Windows/macOS/Linux 桌面稳定契约，Windows=amd64/arm64+WebView2 运行时；发布节奏转 nightly + weekly（v3.1.0-nightly.20261028 已现），**锁定 v3.0.0 tag 不追 nightly**（纪律同 golangci-lint 锁 v2.15） |
| 2 | Go 版本 | 兼容 | wails v3 要求 go ≥1.26；本仓库 go 1.27，go1.27.0 实证编译通过（Build Info: Compiler=go1.27.0） |
| 3 | Windows 零 cgo | **实证通过** | ① 静态：全模块 `import "C"`/`#cgo` 仅命中 darwin/linux 文件（pkg/mac、pkg/events/events_darwin、webkit_linux* 系列），`*windows*.go` 零命中；② 动态：探针 `CGO_ENABLED=0` 构建成功（见 #5） |
| 4 | WebView2 交付形态 | **Go 原生 loader，无 DLL 随包** | `internal/webview2/webviewloader` 全 syscall（kernel32/ole32/version.dll `NewLazySystemDLL`），`UsingGoWebview2Loader=true`；运行时探测系统 Evergreen（本机注册表 HKLM 机器级 156.0.4314.58，Win11 专业版 26300）。「随包 loader vs 系统运行时」决策闭环：系统运行时 + 缺失回退（§五） |
| 5 | 探针构建+冒烟 | **双绿** | `%TEMP%\wails-probe`（三文件 build tag 形状=本设计拟用形状）：Windows 构建 OK（17.5MB）；`GOOS=linux` 交叉构建 OK（**2.3MB，wails 未链入=隔离有效**）；运行 exit=0，日志 `Go-WebView2Loader=true WebView2=156.0.4314.58`、`Environment created successfully`（退出时 unregister class 报错为 Chromium 关停噪声，无碍） |
| 6 | 消息循环并存 | 理论成立，W2 冒烟验证 | 现状 `runApp` 已把 `systray.Run` 放独立 goroutine（独立线程 Win32 消息泵，app.go:296）；wails `app.Run()` 主线程锁（mainthread_windows.go）——两泵各占一线程合法共存；wails 不调 `SystemTray.New` 即无双托盘图标（fyne.io/systray 原样保留） |

Wails v3 顺带盘点到的内置能力（**勿自建**）：单实例（`single_instance_windows.go`）、
WebView2 进程失败恢复（blog "tougher Windows experience" + `processfailed_test.go`）、
`HideOnFocusLost` / `HideOnEscape` 窗口选项、`WebviewWindowOptions.URL` 直载。

API 装配形状（探针验证）：

```go
app := application.New(application.Options{Name: "yardmaster"})
win := app.Window.NewWithOptions(application.WebviewWindowOptions{
    Title: "Yardmaster", Width: 1200, Height: 820,
    MinWidth: 900, MinHeight: 600,
    URL: "http://127.0.0.1:8317/?app=1",
})
_ = win
return app.Run() // 阻塞至退出
```

## 三、依赖边界设计（build tag 分窗）

```
internal/app/
├── window.go            # 无 tag：浏览器窗口方案（现状原样保留，两平台共用）
├── window_wails.go      # //go:build windows：原生窗口装配（本里程碑新增，唯一 wails import 点）
└── app.go               # runApp 内经选择器分派（windows 先试 native，失败回退 browser）
```

- **唯一 import 点**：`window_wails.go` 是全仓库唯一 import
  `github.com/wailsapp/wails/v3/...` 的文件；`!windows` 构建完全不编译
  wails（探针 linux 2.3MB 实证）。
- **断言测试**（W2 落地，双保险）：① CI 步骤 grep `internal/gateway`
  等核心包 `go list -deps` 不含 wailsapp/wails（廉价）；② 单测对
  `go list -f '{{.Deps}}' ./internal/gateway` 断言零 wails（go list 本地
  命令不碰网络，go 缺失则 t.Skip）。
- **禁止事项**：网关核心/控制台前端**不引入** wails runtime.js、不注册
  Service、不用 bindings 生成器——窗口与控制台的关系是纯 HTTP（§四）。

## 四、窗口加载形态（纯 URL，零 JS 桥）

- `WebviewWindowOptions.URL` 直指 `http://127.0.0.1:8317/?app=1`（沿用
  现有 `?app=1` 桌面密度语义与 app.css 设计令牌，控制台零改动）。
- `AllowSimpleEventEmit` 保持默认 **false**——上游源码安全注记明确：
  开启后页面内任意脚本（含 XSS 汇）可发 Wails 自定义事件；我们无
  Service、无 bindings，事件面为零。
- 外部链接（`window.open`/target=_blank）拦截走系统浏览器（W3，机制
  W2 落地时按 v3 事件 API 选型）。
- 不使用 wails AssetServer（控制台仍由网关 embed 提供）——窗口只是
  一个指向本机回环的壳。

## 五、降级路径（WebView2 缺失回退）

1. **启动探测**（windows 路径，`window_wails.go` 内，纯 Go
   `golang.org/x/sys/registry`）：读
   `HKLM\SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`
   与 HKCU 同键的 `pv` 值，非空即视为运行时在位（本机实测键值
   156.0.4314.58）。
2. **双兜底**：探测缺失 → 直接走浏览器窗口（`openAppWindow` 原样）；
   探测在位但 wails 启动失败（error 返回）→ 同样回退浏览器窗口并
   stderr 留痕（可观测，不静默）。
3. 回退路径即 v0.7.0 现行为——降级永不阻塞「二进制直接运行即有界面」。

### 五·甲、平台行为差异（M8-W4 文档化）

矩阵实证（2026-11-02，-trimpath 本地三目标）：windows/amd64 21.1MB
（含 wails，增量 ~4.7MB）、linux/amd64 16.4MB、linux/arm64 15.2MB
（两目标 `go list -deps` 零 wailsapp 断言——build tag 隔离有效，纯 Go
矩阵不变）。release.yml 无需改动：单 job 串行 `CGO_ENABLED=0` 交叉
三目标 + package.sh 打包形态与 app 壳层改动无耦合。

| 维度 | Windows（Wails v3 原生壳） | Linux / 其他（浏览器 `--app` 窗口，M5.5 方案） |
| :-- | :-- | :-- |
| 窗口载体 | WebView2 原生窗口（Go 原生 loader，无 DLL 随包） | 系统 Edge/Chrome `--app` 独立窗口；不可得回退默认浏览器标签页 |
| 关窗语义 | 隐藏到托盘（EventMapping 接管 WM_CLOSE） | 浏览器窗口关闭即关（进程后台继续运行，网关不受影响）；重开走托盘 |
| 窗口状态记忆 | 数据目录 `window-state.json`（尺寸/位置/最大化） | 不适用（浏览器自有窗口管理） |
| 窗口背景色 | 启动时跟系统深浅色（AppsUseLightTheme 探测） | 不适用 |
| 主题切换（侧栏三态） | 生效（localStorage，查看端偏好） | 同左（同一控制台页面） |
| WebView2 缺失 | registry 探测 → 回退浏览器窗口路径 | 不适用（本就是浏览器路径） |
| 外链守卫 | 拦截 + toast 提示 | 同左 |
| 桌面壳依赖 | wails v3.0.0（`windows` build tag 隔离） | 零 wails import（`!windows` 挂点 nil） |

依赖注记：godbus/v5 随 wails 传递升级 5.1.0→5.2.2（systray v1.12.2
声明的最低版为 5.1.0，兼容升级；CI linux race 测试绿佐证）。**Linux
真机 D-Bus 托盘冒烟未实测**（本机无 Linux 桌面环境），与既有未实测
项同类，如实留档待用户实机验证。

## 六、关窗语义决策（W1 定稿）

**关窗 = 隐藏到托盘（hide），退出 = 托盘菜单退出。**

- 理由：app 模式本就是「进程内网关常驻 + 托盘」形态，关窗杀网关会
  中断后台推理/派发会话（M5/M6 派发语义），与托盘常驻心智一致；
  FLClash 式观感亦然。
- 机制候选（W2 按 v3 实际 API 选型）：窗口 close 事件拦截 →
  `win.Hide()`；若拦截不可靠，兜底方案=关窗即退出 + 托盘菜单「打开
  控制台」可再开（行为差异在 app.md/README 如实标注）。
- 窗口状态记忆（尺寸/位置/最大化）持久化到 settings.json 新键（走
  既有 settings 写路径），W3 落地。

## 七、Magpie 设计语言参考（2026-11-02 增补，W3 输入）

> 用户定向：Magpie 界面设计优秀，作为控制台视觉与交互参考。核实结
> 论：**Magpie（yetone，MIT 许可）本身即 Wails 应用**——`index.html`
> 含 `--wails-draggable:drag` 拖拽区标记，纯 HTML/CSS/JS 无框架无构建、
> Go embed 服务（`internal/gui/assets/`），与本里程碑技术栈（Go+Wails
> +embed 控制台）完全同构，等于「Wails v3 + 纯 URL 控制台」路线的一
> 手生产实例背书。两边同为 CSS 令牌体系（我方 M5.5 令牌 vs 其
> app.css），参考成本低。

### 七·甲、布局采纳：侧栏信息架构（2026-11-02 用户定向二次澄清，W3 主项）

> 用户澄清「参考的是软件界面的布局」——Magpie 的侧栏式信息架构正式
> 采纳为控制台（浏览器态与 app 态统一）的布局目标形态。

**目标形态**（Magpie 同构：固定侧栏 + 瘦顶栏 + 内容区）：

```
┌──────────────┬─────────────────────────────────────────┐
│ ▓ Y yardmaster│  总览            [状态徽章] [重载配置] [◐] │ ← 顶栏瘦身为
│ ▓────────────│                                          │    页面上下文+动作
│ ▓ 运行        │                                          │
│ ▓  ◈ 总览     │                                          │
│ ▓ 路由        │                 内容区                    │
│ ▓  ◈ Provider │        （卡片流，表格型页面受益于通栏）      │
│ ▓  ◈ 路由     │                                          │
│ ▓ 观测        │                                          │
│ ▓  ◈ 观测     │                                          │
│ ▓ 自动化      │                                          │
│ ▓  ◈ 派发     │                                          │
│ ▓  ◈ 插件     │                                          │
│ ▓             │                                          │
│ ▓ ◐ 主题  vX  │ ← 侧栏脚部：主题切换 + 版本/状态            │
└──────────────┴─────────────────────────────────────────┘
  ▓=侧栏常暗（工业风锚点，明暗主题下均深色——M5.5 深色顶栏心智迁移至此）
```

**几何与实现要点**（照抄 Magpie 实测参数，出处 app.css:349+）：

- `body.side-nav { --side-w: 212px; padding-left: var(--side-w) }`，
  `<860px` 收窄 184px；`.sidebar` fixed 全高。侧栏可拖宽度边
  （`.side-edge` col-resize）为可砍项。
- 导航项 = 16px 线性图标 + 文字（内联 SVG，Magpie 同款 stroke 风格）；
  hover 药丸底、`.on` 卡片底+描边阴影；分组标签 11px muted。
- **路由机制零改动**：现有 `data-tab`/hash（`#overview` 等）原样保留，
  顶栏 `<nav>` 平移为侧栏列表——JS 切页逻辑不动，纯 HTML/CSS 重排。
- **顶栏瘦身**：状态徽章 + 重载配置 + 主题切换（新增）留在顶栏右侧；
  h1 页面名随页切换。设置卡片仍留总览页（独立成页为可砍项）。
- **?app=1 密度语义延续**：紧凑密度/全宽/工业风令牌不变；深色表面从
  顶栏迁移到侧栏（两态统一侧栏常暗，明暗主题 ×2 组合不膨胀）。
- 内容区统一通栏卡片流（浏览器态不再 max-width 64rem 居中；若实测
  过宽回退居中，留档 W3 浏览器核查决定）。
- 窄窗折叠回顶栏 tabs（手机/极窄场景）：可砍——桌面窗口 min-width
  900px 不需要。

### 可采纳项（W3 落地，按价值排序）

| # | 项 | Magpie 做法（出处 app.css/index.html） | 我方落点 |
| :-- | :-- | :-- | :-- |
| 1 | 明暗双主题 | `prefers-color-scheme` 自动 + `:root[data-theme]` 手动覆盖 + `<meta theme-color>` 成对；语义色成对定义（`--green/--green-soft` 等，控件在任何表面 4.5:1 对比） | W3「深浅色跟随系统」的实施方案：app.css 令牌层扩展明暗两套 + 切换开关持久化（走既有 settings 写路径）+ WebView2 窗口 BackgroundColour 联动防闪白 |
| 2 | Windows 字体栈 | `:root.win`——正文 `"Segoe UI Variable Text","Segoe UI","Microsoft YaHei UI"`；等宽 `"Cascadia Mono",Consolas,"Microsoft YaHei UI"` 兜底——**防中文落 SimSun 等宽细体陷阱（其 #690）** | app.css 一处改动；对本工具大量「中文+等宽数字表格」的观感是关键细节 |
| 3 | 选择纪律 | 内容（表格/日志/错误/hint）`user-select:text`、控件（按钮/导航/标签/菜单）不可选——「bug 报告需要文本」 | 与 trace 详情/派发输出/插件日志尾场景完全吻合；全局 `:where()` 低特异性规则对齐 |
| 4 | 色盲安全色板 | Okabe–Ito 定性系列，明暗两套 + `data-colorblind` 切换，相邻系列色相/亮度分离 | 成本/观测页图表与徽章色（当前 bar-fill 三色可纳入同一纪律） |
| 5 | 焦点环 | `:focus-visible { outline: 2px solid var(--accent); outline-offset: 1px }` | 已有 focus ring 语义，实现方式对齐 |

### 不采纳 / 留档项

- i18n 框架（`data-t` 翻译属性）：控制台中文单语，非目标。
- 无边框窗口 + 自绘标题栏（`--wails-draggable`）：W1 已定系统标题栏，
  自绘留档可砍。
- Omarchy 主题变体、PWA manifest / mobile 适配：非目标。
- 侧栏可拖宽度、导航项事件圆点（has-dot）、面包屑 crumb：细节留档，
  W3 按余量取舍。
- 设置独立成页（侧栏脚部入口）：现设置卡片留总览页，页数增长后再拆。

### 许可纪律

MIT（Copyright (c) 2026 yetone）：设计模式借鉴无义务；若逐字复用事
实性片段（色板值/字体栈），app.css 内加一行出处注释（如
`/* 色板参考 magpie (MIT, © yetone) */`）；不做整文件拷贝——保留自
有令牌命名与调度台工业风基调，Magpie 是「参考系」不是「皮肤来源」。

上游跟进（2026-11-02 用户指令）：源码 `github.com/yetone/magpie.git`，
发布流 `github.com/yetone/magpie-releases/releases`；基线 **v0.1.2699**
（2026-11-02）。每次 M8 桌面壳/控制台相关工作开跑前抓 releases 页比
对基线，增量中与桌面壳/控制台布局相关者回填本节与 §9.9 排期，无关
则仅滚动基线号。本地 `magpie-main/` 为参照快照（非 clone）。

### W3 增补清单（并入 §九 W2 清单之后的执行序列）

1. **侧栏信息架构**（§七·甲）：HTML 导航平移（data-tab 机制不动）+
   侧栏样式（常暗/分组/图标/选中态）+ 顶栏瘦身 + 通栏内容区；浏览器
   实测核查（明暗两态 × app/浏览器窗口态 × 六页遍历）。
2. app.css 令牌层扩展：明暗两套语义色 + `:root.win` 字体栈（含等宽
   YaHei UI 兜底）+ Okabe–Ito 图表色。
3. 主题切换：跟随系统（默认）+ 手动覆盖持久化 settings 新键；WebView2
   `BackgroundColour` 与主题联动；入口在侧栏脚部。
4. 选择纪律 + `:focus-visible` 全局规则。
5. 改 web 资产后重新 `go build`（embed 生效前提，M5.5 教训）。

## 八、成本与风险

| 项 | 评估 |
| :-- | :-- |
| 二进制体积 | 现 yardmaster.exe 13.7MB；探针 wails delta ~15MB，Windows 产物预估 ~28MB。Linux 产物零增长（2.3MB 探针证明）。可接受 |
| 依赖树 | wails v3 go.mod 依赖庞大（CLI 工具链 nfpm/bubbletea 等），但 Go 只编译被 import 的包——探针实测仅拉 go-ole/spew/difflib 少量新增；`go mod tidy` 后以 go.sum 实际为准 |
| v3.0.0 新鲜度 | 发布仅一周（2026-10-26）。缓解：锁 v3.0.0；窗口路径带浏览器回退（§五）= 原生壳故障永不阻塞主功能；W2~W6 每周真机冒烟 |
| 双消息循环 | 线程隔离合法（§二#6）；风险低，W2 冒烟重点验证窗口+托盘+网关三者生命周期（尤其退出顺序：tray exit → app.Quit + 网关 shutdown 无悬挂） |
| DPI/多显示器 | 未实测（§十），W3 状态记忆落地时真机验证 |

## 九、W2 落地清单

1. `internal/app/window_wails.go`（`//go:build windows`）：registry 探测 +
   `runWailsAppWindow(url) error` 装配 + 关窗拦截。
2. `app.go` runApp 分派：windows 先 native 后 browser 回退；Linux 原样。
3. 依赖边界断言（§三双保险）。
4. `go get github.com/wailsapp/wails/v3@v3.0.0` + tidy（go.sum 锁定）。
5. 冒烟：窗口+托盘+网关三生命周期、退出顺序、回退路径日志、
   `serve`/`console` 回归。

## 十、未实测项（如实标注）

- ~~WebView2 运行时缺失真机回退演练~~ **已完成（2026-11-03，W6）**：
  以应急开关 `YARDMASTER_NO_NATIVE_WINDOW=1` 模拟缺失（注册表改键方案
  需管理员权限且风险高，环境变量等价且顺带成为用户应急出口）——回退
  浏览器窗口真机实证，见 W6 锚点。
- ~~深浅色跟随系统（v3 WindowsOptions 主题字段 W2 核实后定）~~ **已
  落地（W3）**：CSS 双主题 + 初始窗口背景色按 AppsUseLightTheme 探测。
- 窗口状态记忆的 DPI 缩放/多显示器表现（真机日常覆盖）。
- 托盘菜单真机点击（「打开窗口」重开 / 「退出」收尾顺序）——待用户
  日常确认。
- 托盘与 wails 窗口长期共存稳定性（持续观察）。
- Windows ARM64（矩阵内构建验证，无真机）。
- Linux 侧原生窗口为非目标（webkit2gtk cgo 非目标），浏览器窗口方案
  与 v0.7.0 行为等价由 W4 回归验证；Linux 真机 D-Bus 托盘冒烟留档。
