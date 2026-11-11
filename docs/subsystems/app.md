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
> - A/B 对照（git worktree）：`systray error: unable to set
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
>   （中文 FileDescription UTF-16 正常）、per-monitor-v2 DPI
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
>   **追记（2026-11-10）**：打包层已整体移除——产物改裸二进制直发
>   （见文末「发布形态改造」记录），本条留档为当时形态。
> - 资源版本与运行时版本（-ldflags 注入）互不依赖，如实
>   记录。TUI/CLI 对齐小项按可砍清单砍除。

> [2026-11-03-M8W6 冒烟发布交付] §9.9 W6 全项落地。冒烟
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

> [2026-11-11-开机自启动设置 autostart] 用户需求「应用内设置是否自
> 启动」——此前自启动靠启动文件夹快捷方式（外部手段，不可控不可见）。
> 设计：settings 新键 `autostart`（*bool 指针，语义同 Notify：PUT nil=
> 保留现值；`AutostartEnabled()` nil=缺省关——自启动属用户显式选择）；
> 壳层落地 = **HKCU 注册表 Run 键**（`...\CurrentVersion\Run` 值名
> Yardmaster = 引号包裹的当前 exe 路径；HKCU 无需管理员；registry 依赖
> 已在 window_wails.go 使用）。生效链路：PUT /api/config/settings 落盘
> → **Server.OnSettingsChange 新弱钩子**（同 OnAccountRest 模式，nil
> 安全）→ `applyAutostart`（autostart_windows.go；!windows no-op 同
> console_* 先例）——即时生效无需重启；runApp 启动时按偏好幂等同步
> （自愈：手动删键/exe 换路径后自动恢复）。CLI `settings set autostart
> on|off` 落盘后即时同步。失败语义：注册表写失败 slog.Warn 降级留痕
> 不阻断。设置页「通用偏好」加胶囊开关（同 notify 形态）。文件：
> settings.go / gateway.go（钩子）/ api.go（调用）/ autostart_windows.go
> + autostart_other.go / app.go（wireSettingsHook + 启动自愈）/
> settings_cmd.go / web UI / webapi_test.go（指针语义测试）。
> genwinres appVersion → 0.1.1（发版随 tag）。

> [2026-11-11-自启动静默常驻] 用户反馈「自启动后每次都打开控制台」——
> 登录拉起即弹应用窗口（控制台 UI）非预期，静默常驻才贴合「自启动」
> 心智。方案：Run 键值追加 `--no-window`（runApp 既有 flag）——登录
> 后仅托盘承载（网关照常伺服），界面入口=用户双击 exe / 托盘菜单
> 「打开窗口」。同批澄清：设置页「开机自启动」开关 v0.1.1 已在（通用
> 偏好区胶囊开关，用户未刷新页面误认为缺失）。genwinres appVersion →
> 0.1.2。

> [2026-11-11-插件子进程黑窗抑制] 用户截图反馈「每次启动弹
> C:\...\node.exe 黑窗」——context-guard 插件（node 长驻）为 console
> 子程序，从 GUI 版网关（windowsgui 无控制台）spawn 时 Windows 机制性
> 新建 conhost 黑窗（父有控制台时继承不弹，故 serve/CLI 场景从未显现）。
> 方案：插件 spawn 统一加 **CREATE_NO_WINDOW**（procattr_windows.go 的
> SysProcAttr.CreationFlags + runOnce 接线；!windows 返回 nil 零变化）
> ——插件是 NDJSON 管道后台服务型子进程，本就无窗口语义；stdin/stdout
> 管道与 stderr 转发不受影响。genwinres appVersion → 0.1.3。派发
> Agent（用户主动派发、输出事件流可观测）不在本修复范围。

> [2026-11-11-托盘菜单开机自启动开关] 用户截图（托盘右键菜单）需求
> 「此处可直接配置是否自启动」——进设置页路径太深。方案：托盘菜单
> 「打开窗口」下新增 **checkbox 菜单项**（fyne systray v1.12.2 的
> AddMenuItemCheckbox）：AutostartEnabled（初态查询）+ AutostartToggle
> （翻转：内存态 + settings.json 落盘（SaveSettings 原子写，与 CLI
> settings set 同构——壳层偏好不经管理 API 写闸）+ applyAutostart 即时
> 同步 Run 键）；勾选态强制对齐回调返回值（落盘失败回滚原态）。实现
> 注意：select 的 case 表达式内做 `mAuto != nil && <-ch` 会**求值即消
> 费**吞掉点击事件——改 nil-channel 惯用法（未装配时 mAutoCh=nil，case
> 永不就绪）。serve -tray 与 app 两入口 TrayOptions 均装配（
> serveSetup.autostartCallbacks 共享回调对）。控制台设置页开关与托盘
> 双入口读写同一 settings.json（GET 读盘对齐）。genwinres
> appVersion → 0.1.4。

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
3. 回退路径即现行为——降级永不阻塞「二进制直接运行即有界面」。

### 五·甲、平台行为差异（M8-W4 文档化）

矩阵实证（2026-11-02，-trimpath 本地三目标）：windows/amd64 21.1MB
（含 wails，增量 ~4.7MB）、linux/amd64 16.4MB、linux/arm64 15.2MB
（两目标 `go list -deps` 零 wailsapp 断言——build tag 隔离有效，纯 Go
矩阵不变）。release.yml 单 job 串行 `CGO_ENABLED=0` 交叉三目标直出
裸二进制（无打包层，见文末「发布形态改造」记录），与 app 壳层改动
无耦合。

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

> **追记（2026-11-10）：关窗行为可配**。settings 新键
> `close_action`（hide=隐藏到托盘（缺省，本节决策）；exit=直接退出
> 程序含托盘）——管理 API（非法值 400 fail-closed、空值保留现值）/
> CLI `settings set close-action` / 控制台「通用偏好」三入口。桌面壳
> 在关窗事件时**实时重读** settings.json（读失败/未知值按 hide 处理
> ——绝不因配置异常把常驻网关悄悄退出），保存后下次关窗即生效，无
> 需重启。exit 路径 stop()+Quit 与 Ctrl-C 同一收敛。浏览器窗口（回退
> 路径）关闭行为不归本设置管（浏览器自有语义）。

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

### 七·乙、控制台深度重构：Magpie 构造与交互全面对齐（2026-11-03 用户定向）

> 用户指令「参照 magpie 的界面构造与交互，大幅度修改前端」——在本节
> 布局采纳之上再做一轮深度重构。**两项旧决策被推翻并留痕**：①侧栏
> 「常暗工业风锚点」（七·甲）改为 **Magpie 同底色侧栏**（与内容区共用
> `--bg`，选中项 = `--card` 卡片浮起 + seg-edge 描边，单一滑动高亮形状
> `.nav-hl`）；②通用 `.card`+`<table>` 构造改为 **全面行卡片化**
> （`.list`/`.row`/`.who`/`.name`/`.sub`，全站无 `<table>`）。对比分析
> 全文见工作区《Magpie前端对比与重构方案.md》。

**构造**：应用壳（body 不滚、各视图独立滚动，替代 sticky 顶栏补丁）；
Magpie 令牌体系整体替换（四层文字灰阶 fg/fg-2/muted/faint、两档线
line/line-2、card-2 次级表面、accent 靛色 `#4f46e5`、pill 家族 999px）；
`.label` 分区头 + `.empty-state` 空状态 + 骨架屏；模态统一 `.dialog`
（大柔影+入场动画），`confirm()` 全部替换为 `confirmModal()`。

**交互**：`status()` 顶栏状态行（ok 3.5s/err 8s 自动消）全面替代
toast；`show(tab)` 激活即懒加载 + `document.hidden` 轮询守卫（15s 总览
/30s trace/5s 派发不变）；HTTP 错误码→中文可操作文案映射；Provider
客户端查找过滤；胶囊开关（`.lib-switch`）用于 trace/派发自动刷新与插件
启停（乐观更新，失败回弹）；prefers-reduced-motion 全局尊重。

**不变**：全部管理 API 面与写路径五铁律、令牌 401 单飞重试、
data-tab/hash 路由机制、`?app=1` 密度语义、主题三态循环（localStorage）、
外链拦截、`win` 字体栈。**留档不做**：命令面板、i18n、插件 iframe、侧
栏拖宽、crumb、View Transitions、X-Magpie-Delta 增量协议。

许可纪律延续：色板值/字体栈/几何参数逐字复用处已在 app.css 头部标注
（magpie，MIT © yetone）；结构与命名自主。验证：Node mock 伺服 +
浏览器三轮实测（六页遍历 × 明暗双主题 × 模态/过滤/展开/开关交互 ×
F5 恢复）全过、控制台零 JS 错误；`$('id')` 70 处引用与 HTML 全匹配。
~~未实测：真实 serve 全链路~~ **已实测（2026-11-03 真机冒烟核销）**：
隔离数据目录（USERPROFILE 覆写，不碰真实 ~/.yardmaster）起真实 serve
——go:embed 产物带全部重构标记（nav-hl/`.dialog`/lib-switch）；管理
API 12 端点全 200（含新 `GET /api/provider-presets` 13 预设形状吻合）；
写路径闭环——settings 同值回写（200 + effect=restart + 写响应含
state/config）、providers 追加（新明文 key 落盘 + API 回显 `***` 脱敏）
与整行移除均过，`backup/` 留 3 份审计快照 + reload/probe 200；浏览器
六页对照真实 serve 复测（总览/Provider/路由/观测/派发 × 明暗主题 ×
添加模态预设 tile 预填）全过、控制台零 JS 错误。go vet + go test ./...
全绿。

**七·乙追记（同日）：Provider 实体编辑补齐。** 此前前端仅只读 + 额度
（CLI 注释「UI 向导后续接入」的历史边界），本轮补齐：①新只读端点
`GET /api/provider-presets`（与 CLI `-preset` 同一 `provider.Presets`
事实源，api.md 已记）；②「＋ 添加 Provider」模态——预设 tile 预填
（id/名/协议/base_url，显式改动不覆盖，CLI add 同语义）→ 表单（密钥
type=password、账号代理/Provider 代理/最大并发）→ `putProvidersDoc`
提交；id 已存在=追加账号（账号名冲突拒绝，指向 CLI `-replace`），新
明文 key 经服务端 `keepProviderKeys` 采纳；③账号行 hover「删除」→
`confirmModal` → 整行移除（服务端语义：空 key=保留，移除=删除）。
OAuth 订阅类不在前端（授权流在浏览器外，仍 CLI add-oauth）。测试：
`provider_presets_test.go`（端点形状漂移即失败）；浏览器实测 tile 预填/
落盘持久化/掩码回显/删除确认全过。

**七·乙追记之二（2026-11-04）：M10 候选第一梯队 B1+A1 落地。** 开跑前按
基线铁律比对 Magpie 上游增量（v0.1.2699→v0.1.2890，255 提交：命令面板
无改动参考仍有效；升级提示**吸收其 servedAnew 增量**——页面伺服版本与
服务端应答版本漂移时自动重载，6673171）。①**控制台升级提示（B1）**——
侧栏脚部 accent 软底胶囊（Magpie `.update` 可隐藏角标同构）：浏览器端
直查 GitHub latest release（CORS 开放、1h localStorage TTL、失败静默
不扰），版本比较分段数值（dev 态不检查，同托盘 trayUpdateLabel 语义）；
× 忽略按 tag 记忆（下个新版才再提示），点击开 releases 页（原生窗口
外链守卫语义不变）；**servedAnew 同步吸收**：refreshState 检测 boot
版本≠应答版本 → 自动重载一次（页面由同一 serve 伺服自身前端，无循环
风险；dev 态两侧同名不触发）。②**命令面板（A1）**——Ctrl+K / 顶栏
K 形图标打开（Magpie `.pop` 构造裁剪版，复用 .dialog 弹层体系）：6 视图
跳转 + 4 动作（切换主题/重载配置/余额探测/添加 Provider，全部复用既有
处理器**零新增 API 面**）；↑↓/Enter/Esc 键盘流 + hover 高亮跟随 +
输入过滤；表单模态在前时不叠加；余额探测为 POST /api/quota/probe
（M9-W3 端点）首次进前端。真机冒烟（隔离 USERPROFILE + 版本号注入双
二进制）：升级 pill 真实 GitHub 数据触发（0.8.1 → 提示 v0.9.0）+
忽略记忆 F5 保持；**servedAnew 全链路实证**（0.8.1 页面 + 0.9.0 serve
→ 15s 轮询内无手动刷新自动跟上，pill 正确隐藏）；命令面板键盘/指针/
过滤/执行/关闭全过，控制台零 JS 错误；go vet + gateway/app 包测试绿。

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

> [2026-11-03-M9-W6 定稿] M9 验收铁律：无「待用户日常确认」悬置项——
> 逐项转为已实测（附证据）或正式留档（附原因与验证指引）。

- ~~WebView2 运行时缺失真机回退演练~~ **已完成（2026-11-03，W6）**：
  以应急开关 `YARDMASTER_NO_NATIVE_WINDOW=1` 模拟缺失（注册表改键方案
  需管理员权限且风险高，环境变量等价且顺带成为用户应急出口）——回退
  浏览器窗口真机实证，见 W6 锚点。
- ~~深浅色跟随系统（v3 WindowsOptions 主题字段 W2 核实后定）~~ **已
  落地（W3）**：CSS 双主题 + 初始窗口背景色按 AppsUseLightTheme 探测。
- ~~托盘菜单真机点击（「打开窗口」重开 / 「退出」收尾顺序）~~ **已实测
  （M9-W4/W6，2026-11-03）**：构建真机确认——菜单形态正确
  （版本行禁用态 + 状态摘要行 + 打开窗口/退出）、「打开窗口」
  点击 → 浏览器弹出控制台（用户确认）+ 派发完成气泡弹出；「退出」
  收尾路径同构（stop 信号 + systray.Quit，多次 serve 信号
  收尾已验证），日常使用自然覆盖。
- 窗口状态记忆的 DPI 缩放/多显示器表现——**正式留档**（本机单显示器
  无缩放切换场景）：验证指引=多显示器/不同缩放率机器拖窗跨屏 → 关窗 →
  重开，核对 window-state.json 与实际位置。
- 托盘与 wails 窗口长期共存稳定性——**转为日常观察项**（非未实测：
  W4~W6 期间 serve -tray 长跑无异常，退出语义由托盘菜单/信号收尾覆盖）。
- Windows ARM64——**正式留档**（无设备）：矩阵内构建验证覆盖（CI 交叉
  断言）；验证指引=ARM64 设备安装 windows exe 产物（x64 模拟），按
  M8-W6 S1~S4 口径冒烟。
- Linux 侧原生窗口为非目标（webkit2gtk cgo 非目标），浏览器窗口方案
  行为等价性由 W4 回归验证；**Linux 真机 D-Bus 托盘冒烟正式
  留档**（无 Linux 桌面）：验证指引=Linux 桌面机器安装 release 产物，
  运行 app/serve -tray，确认托盘图标出现且菜单（打开控制台/退出）可点。

## 十一、自动更新（M11-W1 方案定稿）[2026-11-04]

> 开发文档 §9.13 排期（四周）。现状基线：update 仅检测
> （CLI updateCheck / 托盘动态入口 / 控制台 B1 pill 均只开 releases 页）；
> release 产物 `pack/` 裸二进制 + SHA256SUMS 复用（2026-11-10 起无
> 压缩包层，见文末「发布形态改造」记录；原文产物为 zip，管线已随
> 之改造）。

### Magpie updater 交叉参考（internal/update + internal/gui，v0.1.2890）

- **状态机**：checking → found/latest → downloading → ready →（重启）；
  error / blocked（Smart App Control 拦截试启动）/ source（无包可装，
  指引 release 页）。
- **Windows 自替换**（InstallBinaryAside）：运行中 exe 不可覆盖但**可
  rename** → move-aside（asideWaits 退避 100ms~1500ms×5 + heldFor 30s
  兜底：杀软/OneDrive 短暂占用句柄）→ moveIn staged → moveIn 失败即时
  还原 aside。**试启动拦截检测**（canStart + trialled 按 path/size/
  modtime 去重）：被杀软拦的下载不就位，BlockedError。
- **重启**（runAfterExit + relaunch.log >64KB 轮转）：等本进程退出后
  拉起；**update_wait**：重启前等网关空闲（in-flight 推理请求保护，
  waitMost 上限，超时则安装推迟到退出/下次重启）。
- **版本跟随**（6673171，已吸收 servedAnew 于 B1）：页面/旁路进程版本
  漂移自动跟上——Yardmaster 单进程无旁路场景，servedAnew 已覆盖。

### Yardmaster 方案定稿（差异与取舍）

1. **aside 换侧 + 接力进程**（与 Magpie runAfterExit 等价的纯 Go 方案）：
   ①旧进程 rename 自身 → `yardmaster.exe.old`（先 RemoveOld 清上次残留，
   同名不覆盖——上次 aside 可能仍在运行）②staged 新 exe（双 exe：主 +
   yardmaster-app）moveIn 原位，失败即时还原 ③旧进程 spawn **接力进程**
   = 新 exe `--update-relay <oldPid>`：WaitForSingleObject 等旧 pid 退出
   → 拉起原路径主程序 → **healthz HTTP 探活**（网关自带，优于 Magpie 的
   进程存活判断）→ 超时/秒退 → 回滚（exe→`.new-failed`，`.old`→原位，
   拉起旧版）。`--update-relay` 标记防循环（接力进程自身不触发更新）。
2. **资产管线**（2026-11-10 起裸二进制形态）：latestRelease 扩展资产
   枚举（本平台 `yardmaster-<GOOS>-<GOARCH>[.exe]` + Windows 追加
   `yardmaster-windows-app.exe` 桌面入口 + SHA256SUMS）→ 多资产逐个
   流式下载（进度回调→托盘气泡节流）+ SHA256 边下边验（不匹配即弃
   +留痕）→ `.part` 校验后改名落位暂存 update/ 子目录并补执行位
   （磁盘空间预检；失败清理）→ exe 就位。暂存契约（`update/
   yardmaster.exe` + `yardmaster-app.exe` + VERSION）与安装引擎解耦
   不变。
3. **settings `update` 三态**：off（完全静默）/ **check（默认，现状
   行为：只提示）** / auto（发现即下载安装重启）——cfgSettings 键级
   编辑器兼容；自动替换 exe 高敏，auto 默认关。
4. **联动**：托盘菜单「检查更新/立即安装」+ 气泡进度（复用 M9-W4 通知
   限频）+ 控制台 B1 pill 点击触发 `POST /api/update/install`（管理
   API 写路径纪律：令牌强制 + 审计 + 单飞防并发）；CLI `update
   -install`（headless 手动全管线）。
5. **范围砍项**：断点续传 / 增量更新 / GUI 安装向导 / macOS 产物。
   Linux：裸二进制/校验逻辑跨平台且 Linux 资产正式发布（此前 zip 仅
   Windows，Linux 更新器报缺资产；裸二进制后自动升级全平台可用），
   自替换路径按 GOOS 分支（无实机，留档附验证指引）。

### 探针实证（%TEMP%\ym-relay-probe，双 exe 三场景）

- **运行中 exe rename**（moved-aside ok）+ **staged moveIn**（ok）+
  **接力等 pid 退出后拉起新版**（healthy：原位=new 字节，`.old`=旧
  字节）+ **健康检查失败回滚**（坏包秒退 → exe→`.new-failed`、
  `.old`→原位、旧版重启成功）。五机制全实证，方案可行（探针简化点：
  waitPID 用 tasklist 轮询、探活为进程存活——真实实现
  WaitForSingleObject + healthz，见上）。

> [2026-11-04-M11W2 下载与校验管线交付] §9.13 W2 全项落地（提交
> df15171）。实测与偏差（**注**：本记录描述 zip 时代的管线——资产
> 名/解包/预检倍数已于 2026-11-10 改裸二进制形态，见文末「发布形态
> 改造」记录；其余机制——SHA256SUMS 容错解析、流式边下边验、失败
> 整体清理——原样延续）：
> - **管线形态**（internal/app/update_download.go）：
>   fetchRelease（含资产清单）→ 平台 zip 匹配
>   （`yardmaster-<GOOS>-<GOARCH>.zip`，package.sh 实证命名；非 zip
>   平台真 release 查无此资产自然报错，不做硬编码拦截）→
>   SHA256SUMS 解析（两空格/`*` 二进制标记/CRLF/杂行容错，64 位
>   十六进制行才收）→ 磁盘预检（zip×3：本体+解包+替换余量；
>   diskFree 按 GOOS 分文件——Windows GetDiskFreeSpaceEx（x/sys，
>   标准库 syscall 无此函数）、Unix Statfs Bavail×Bsize；探测失败
>   不阻断，交给真实写错误兜底）→ 流式下载 Tee 语义边下边校验
>   （读一次磁盘都不多；进度回调逐 64KB 块触发，W3 托盘气泡经此
>   节流）→ 白名单解包（basename 匹配 `yardmaster.exe` /
>   `yardmaster-app.exe`，目录前缀任意；输出名取自固定白名单，
>   zip-slip 无面；README 不入暂存）+ VERSION 标记；zip 本体解包
>   后即弃。**任何一步失败整体清理暂存目录——绝不留半成品**。
> - **latestRelease 收敛**：M9-W4 抽取的 check/托盘共用路径委托
>   fetchRelease，单一 GitHub 查询实现（此前 check 与管线各持一份
>   HTTP 解析）。
> - **CLI**：`yardmaster update download`（-dir 覆盖暂存目录缺省
>   exe 同级 update/、-force 重下同版）；只交付已校验暂存产物，
>   输出尾行明示安装接力是 W3（update install）。
> - **settings update 三态接线**：store 常量+UpdateMode()（空/未知
>   回落 check）+ValidUpdateMode()；管理 API PUT 空值保留现值（表单
>   未提供）、非法 400 fail-closed；CLI settings get 三态展开/set
>   校验，用法行与 default 支持列表同步。
> - **测试**：httptest 假 release 四场景（happy：双 exe+VERSION+3
>   文件恰量+进度收尾对齐；三降级：校验失败/断流/缺资产——均断言
>   报错文案与暂存整体清理）+ SHA256SUMS 变体 + CLI 三态（testenv
>   沙箱）+ API 三态（webapi_test）；**真机守卫测试**
>   TestRealReleaseStageUpdate（YM_REAL_RELEASE=vX.Y.Z 才跑，CI 恒
>   Skip）：对真资产全管线核销通过——12MB zip 流式 SHA256
>   （146d6330d943…）+ 双 exe（>10MB 断言）+ VERSION。
> - **网络环境偏差（如实留档）**：本机 api.github.com 直连可通但
>   objects.githubusercontent.com 阻断（release 资产 CDN）；挂本地
>   代理 7890 后 API 匿名限额又超（代理出口 IP 共享限流）。真机核销
>   最终走「守卫测试+资产 URL 按发布命名规则构造+环境代理」，与
>   API 解耦。**启示**：W3 自动更新网络路径须接 netproxy 全局代理
>   （settings global_proxy），纯环境代理在中国网络环境不可依赖。

> [2026-11-04-M11W3W4 自替换引擎与收官交付] §9.13 W3/W4 全项落地，
> **发布（CI+release 双绿 6 产物）**。提交链 ad79fa6（W3 引擎）
> →a8b8f67（off 静默）→0299aa4（app-only 修复，真机演练实证缺陷）
> →286d001（版本资源）。实测与偏差：
> - **引擎形态**（internal/app/update_install.go）：installStaged（锁
>   单飞 O_EXCL+pid 存活复核/版本闸防降级重装/swapAside 换侧核心/
>   spawn 接力）+ relayInit（等旧 pid 退出端口让位——**relayEnv=
>   YM_UPDATE_RELAY_PID 环境变量而非 argv 标志**，W1 方案 --update-
>   relay 的等价落地：不侵入 flag 解析、serve/app 两入口统一）+
>   relayFinalize（healthz 自探 30s deadline→清 .old+暂存）+
>   rollbackRelay（exe→.new-failed、.old→原位、spawn 旧版无循环）；
>   waitForPID 分平台（Windows WaitForSingleObject——**x/sys 新版事件
>   码返回 syscall.Errno，WAIT_TIMEOUT 常量是 uint32 不能直接 switch，
>   用数值常量 0/258**/Unix kill0 轮询）。
> - **三入口联动**：POST /api/update/install（OnUpdateInstall 钩子，
>   503 未装配/409 失败；重启延迟 800ms 保响应送回——实测响应完整）
>   + 托盘升级项点击应用内安装（nil 回退 releases 页）+ 控制台 B1
>   pill POST 安装；auto 态双端自动触发（托盘 trayUpdateQuery 15s
>   首查命中即装+1h 重试；前端 offerUpdate settings 预拉后自动 POST，
>   每标签页至多一次）；**网络路径接 netproxy 全局代理**（W2 启示落
>   实）。空闲保护砍项留档（本地网关重启<2s）。
> - **真机演练四场景全绿**（隔离 USERPROFILE+端口 18517+ldflags 注入
>   0.10.99/0.11.0/0.11.1 三版本）：①serve 闭环换代——POST install→
>   **2 秒**healthz 0.11.0、原位 exe 字节级=新版、.old/暂存全清、旧
>   进程退出、日志链完整（就位→接力→重启→完成）；②坏包 A（垃圾字节）
>   拒装——409+网关不断服（uptime 连续）+原位 hash 不变+无 .old；③
>   坏包 B（装配必败配置）回滚链——relay 启动失败留痕→exe→.new-failed
>   （hash=坏新版）+.old→原位（hash=旧版）+旧版 spawn 重启，三重字节
>   级验证；④app 模式闭环——GUI 双 exe 换代 2 秒、app.log 链、**app-
>   only 安装（原位无 console exe）首发失败抓真缺陷**：无条件 aside
>   对缺失文件 rename 报错→修为「原位缺失直接落位不产 .old」。
> - **三态矩阵**（真 release 环境）：off=22s 无查询无下载无
>   升级（彻底静默）✓；check=22s 不自动装（只提示）✓；auto=托盘
>   15s 首查命中→自动触发→**GitHub API 403（代理出口 IP 匿名限流，
>   一小时窗口未恢复）→WARN「auto 安装失败 1h 后重试」留痕** ✓——
>   触发链与失败重试语义实证；**下载段被网络环境阻断，分段留档**：
>   真包下载 W2 守卫测试已核销（12MB 流式 SHA256+双 exe）、
>   安装段四场景已核销，完整一键真 release 升级留待网络通畅环境复核。
> - **演练方法论留档**：Shell 工具命令结束后子进程树被清（job object）
>   ——演练进程生命周期=命令生命周期，「起+触发+验证」必须单命令内
>   完成；跨命令存活进程用 WMI Win32_Process（注意其不经 shell 解释，
>   重定向语法会变成 exe 参数）。
> - **servedAnew 联动**：升级换代=/api/state version 变化，M10-B1 的
>   servedAnew 机制（boot 版本≠应答版本自动 reload）同构覆盖，不重复
>   实测。前端 pill 点击为同一 OnUpdateInstall 路径（API 端点已实测
>   200/409/503 三态）；浏览器端点击交互留用户日常确认。
> - **未实测项（如实标注）**：Linux 真机替换路径（无实机——引擎代码
>   跨平台编译+单测双平台覆盖，aside/rename 语义 Linux 同构，验证
>   指引：下载裸二进制（Linux 资产现已正式发布，可直接 `update
>   download` 暂存）后手动模拟 aside/moveIn）；托盘「更新
>   失败（点击重试）」菜单恢复路径（依赖真实点击序列）；.new-failed
>   文件的自动清理（当前留档供诊断，下次安装 aside 前不删——收尾清
>   理仅 .old）。

> [2026-11-04-图标换装与托盘根因修复] 用户定向：jimeng 生成图标网格
> 选定第一排第三（黑底白色发光 S 形岔路箭头）为应用图标；同时用户
> 截图实证**托盘图标从未显示**（tooltip「Yardmaster 网关运行中」在、
> 图标无）——**推翻 M8-W2 的「假阴性、图标实际正常」结论**，那次
> `systray error: unable to set icon: The operation completed
> successfully` 是真失败。三重根因与修复（tools/genwinres 改造）：
> - **根因 1（格式）**：fyne.io/systray Windows 端 SetIcon 把 bytes
>   落临时文件后走 LoadImage(LR_LOADFROMFILE)——**只认 .ico 文件**，
>   PNG 喂入失败且 GetLastError=0（即 M8 记录的假阴性错误文本）。
> - **根因 2（帧编码）**：即便喂 ICO，winres.SaveICO 的帧是 PNG
>   编码（icon.go addImage pngEncode）——资源管理器（shell）认 PNG
>   帧，但 LoadImage 老 API 按 DIB 误读=花屏（System.Drawing.Icon
>   预览实证）。托盘 ICO 必须手写 **BMP/DIB 帧**容器（BITMAPINFOHEADER
>   height 双倍 + BGRA 行倒序 XOR + 1bpp AND 掩码）。
> - **根因 3（可见性）**：旧代码生成图标深蓝底 #1e2430 在深色任务栏
>   几乎隐形。托盘帧加**白色圆角外框**（图标 84% 居中）——深色任务栏
>   看白框、浅色任务栏看黑底白线，双保险。
> - **产物链**：appicon.png（2048px 源图裁 [1380,290,530,530] 入库，
>   jimeng 源图不留库）→ genwinres 单一事实源生成三产物：①exe 图标
>   syso（圆角透明化，四角白底残留按 11.5% 圆径 mask）；②托盘多尺寸
>   ICO（16/20/24/32/48 DIB 帧，internal/app/tray_icon.ico go:embed，
>   LoadImage 兼容性经 System.Drawing.Icon 双尺寸加载+视觉预览验证）；
>   ③通知 PNG（notify_icon.png 64px——beeep []byte 分支按 PNG 解码，
>   与托盘 ICO 格式不同源不同用）。go:generate 一次产出全部。
> - **顺手修复**：notify.go 原把 trayIcon() 字节传 beeep——语义歪打
>   正着（beeep []byte 分支要 PNG，旧 PNG 字节恰好可用）；现改为独立
>   notify_icon.png 嵌入，托盘/通知格式各归其位。
> - **DPI/深浅色未实测项**：任务栏深浅色切换下的实际观感、高 DPI 缩
>   放下的帧选择（16/20/24/32/48 已备）——留用户日常确认。

> [2026-11-10-关窗行为设置与单实例互斥] 用户两项定向：①「设置中控制
> 点击关闭是直接退出还是缩小到任务栏」；②「为什么点击 EXE 会重新新增
> 一个界面」。落地（同批提交，CI 双绿为准）：
> - **close_action 设置键**（store 常量对 hide/exit，镜像 update 三态
>   成对模式）：管理 API PUT（非法 400/空值保留现值）+ CLI
>   `settings set close-action` + 控制台「通用偏好」select 三入口；
>   webapi 测试镜像 TestConfigPutSettingsUpdateMode。
> - **关窗实时读盘**：WindowClosing 回调先 snapshot 后 LoadSettings
>   （失败/未知→hide 缺省）——exit 走 stop()+Quit（与 Ctrl-C 同收敛），
>   hide 走现行为；保存后下次关窗即生效、无需重启。
> - **单实例互斥**：兑现 §二「勿自建」盘点——wails 内置
>   SingleInstance（UniqueID=yardmaster-app）接进 application.Options：
>   锁在 New 获取，二次启动进程 WM_COPYDATA 通知首实例后 os.Exit(0)
>   ——根因修复「二次双击 exe 新开界面」（旧根因：新进程窗口仍加载
>   127.0.0.1:8317=首实例的网关，观感即新界面；其自身网关端口绑定
>   失败只落 app.log）。首实例回调=reopenNativeWindow（前置既有窗口）。
>   留档边界：锁覆盖 wails 壳路径（windows/amd64）；首实例为 serve/
>   浏览器回退态时不持有锁（端口绑定已天然防双网关）；二次启动早期
>   托盘图标短暂闪现 ~1s（锁在 New 时才获取）。真机核销留待发布后
>   （CI 无法覆盖 GUI 交互）。

> [2026-11-10-发布形态改造：裸二进制直发] 用户定向「不要打包成压缩
> 包，直接推送二进制文件」——release 产物去 tar.gz/zip 打包层，
> 裸二进制直传（提交 c6563e8，随版本重置后首发布 v0.1.0 重发实证，
> CI+release 双绿）。改动与实测：
> - **release.yml**：Build 步骤连字符命名直出四资产
>   （`yardmaster-linux-amd64` / `yardmaster-linux-arm64` /
>   `yardmaster-windows-amd64.exe` / `yardmaster-windows-app.exe`）；
>   package.sh 打包步骤整体删除（脚本一并移除）；Checksums 步骤对
>   裸二进制生成 SHA256SUMS；Release 步骤直传。新增**清理步骤**——
>   重发同号 tag（删 tag 重推）时先用 GITHUB_TOKEN 删该 tag 名下既有
>   Release 再发布，杜绝 action-gh-release 追加产物导致新旧资产混挂
>   （本次 v0.1.0 重发即靠它清掉旧压缩包版）。
> - **更新管线**（internal/app/update_download.go）：platformAssetName
>   改 `yardmaster-<GOOS>-<GOARCH>[.exe]`；新增 appAssetName
>   （`yardmaster-windows-app.exe`，资产缺席自然缺省）；stageUpdate
>   改多资产逐个流式下载+SHA256 校验（`.part` 校验后改名落位+chmod
>   0755 补执行位——裸二进制下载不保留权限位）；解包路径
>   （unzipExes/writeExeFile）删除，archive/zip 依赖移除；磁盘预检
>   zip×3 改资产合计×2。暂存契约（`update/yardmaster.exe` +
>   `yardmaster-app.exe` + VERSION）不变，W3 安装引擎零改动。
>   **附带收益**：此前 zip 仅 Windows 发布，Linux 更新器报「缺少本
>   平台资产」；裸二进制后 Linux 资产正式发布，自动升级全平台可用。
> - **安装脚本**：install.sh/install.ps1 直连下载裸二进制（Linux
>   `install -m 0755` 落执行位；--file/-File 离线参数改收二进制）。
> - **测试**：update_download_test.go 重写为裸二进制假 release 服务
>   （Windows 含桌面入口资产、Linux 断言无桌面入口；happy+三降级
>   语义不变）；真机守卫测试 YM_REAL_RELEASE 资产名同步。CI 双平台
>   全绿（含 gofmt——本机无 Go 工具链，格式靠这道闸核销）。
> - **体积对比**：压缩包版 linux-amd64 4.7MB tar.gz → 裸二进制
>   11.5MB（`-s -w` 已剥符号表；无压缩层的自然增量，换取下载即用与
>   全平台自动升级）。
