# 踩坑记录

开发过程中遇到过的问题、根因和处理方式，按领域分类。大部分来自 realme RMX6699（Android 16 / ColorOS / 高通，KernelSU）上的实测。适配其他机型时请结合 [PORTING.md](PORTING.md) 一起看。

每条的格式：**现象** → 原因 → 处理。

---

## 1. Go 引擎与 root 辅助进程

- **Go 共享库里读不到环境变量** → bionic 加载 Go 库时环境变量为空。由系统绑定服务拉起的 App 进程里，Go 读 `/proc/self/environ` 也拿不到内容，`BOOTCLASSPATH` 为空，root 辅助进程（`app_process`）启动后在 RuntimeInit 阶段静默退出，报 `root helper exited (status 0)`。→ App 创建引擎前把 `System.getenv()` 逐个传给 `Mobile.setEnv`；启动脚本在缺 `BOOTCLASSPATH` 时向 stderr 报警。
- **App 进程里找不到 `su`** → `PATH` 可能为空，`exec.LookPath("su")` 不可靠。→ 按绝对路径逐个查找（`core/sim/helper.go` 的 `findSU`）。
- **以 uid 0 调 telephony 被 AppOps 拒绝** → 系统会校验调用方包名。→ RootHelper 以 `su 1000`（system 身份）运行，调用时包名用 `android`。
- **反射找不到 `IccLogicalChannelRequest`** → 实际类名是 `com.android.internal.telephony.IccLogicalChannelRequest`。→ 从 `iccOpenLogicalChannel` 的参数类型反推类（`RootHelper.paramClass`）。
- **同步调用手动选网卡死 20 秒以上** → telephony 在基带搜网期间阻塞，引擎超时，辅助进程卡住，界面报「无法启动 root 辅助进程」。→ 这类操作放到 RootHelper 的后台线程执行并立即返回。
- **网络切换时 Go 引擎崩溃（SIGABRT）** → 墓碑文件里没有 Go 栈。→ 用 `debug.SetCrashOutput` 把 panic 写到 `files/logs/go_crash.log`，下次启动摘入 App 日志。抓到的原因是：注册成功后计数清零，断线时 `backoff[attempt-1]` 取到下标 -1。→ 改为 `max(attempt, 1)`。
- **新会话被旧会话的「已停止」状态覆盖** → `Engine.Start` 替换守护循环时，旧循环收尾会把 UA 置空并发出 `idle`。→ 先等旧循环退出；并且**先**给新会话分配代号（gen）再停旧循环，旧循环发现自己不是当前会话就不发 `idle`。之前顺序反了，每次重启都会短暂撤销系统的「语音在服务」。
- **App 被拉起时引擎启动两次** → 前台服务自恢复和系统开关回调各启动一次，第二次打断第一次的 EAP。→ 同一张卡已在运行时跳过。

## 2. IKEv2 / ePDG

- **随机端口、SPI 越界** → 端口必须小于 65536；部分 P-CSCF 按有符号数解析 SPI，必须小于 2³¹。
- **ePDG 进入防洪模式后一直连不上**（`IKE_SA_INIT: context deadline exceeded`）→ 收到 COOKIE 重发时，旧代码重新生成了 Nonce、DH 和随机的 NAT 探测哈希，cookie 校验失败被对方静默丢弃。→ 按 RFC 7296 §2.6，Nonce 和 NAT 哈希只生成一次，DH 只在 INVALID_KE_PAYLOAD 换组时重建。
- **IKE 私有错误码名称对不上** → 之前的码表是错的。→ 按 TS 24.302 表 8.1.2.2-1 更正：9000 NON_3GPP_ACCESS_TO_EPC_NOT_ALLOWED、9001 USER_UNKNOWN、9002 NO_APN_SUBSCRIPTION、9003 AUTHORIZATION_REJECTED……
- **EAP 通过后被回 9003（EE 系卡：CTExcel、CMLink UK）** → 运营商按接入地区拒绝，国内 IP 不行，走海外出口就正常。→ 不是程序问题，需走代理且代理要支持 UDP。
- **EAP-AKA' 的永久身份格式** → 按 TS 23.003 用 `6` 前缀。
- **ePDG 标准域名解析不到（Globe）** → 实际 ePDG 是 `weconnect.globe.com.ph`。→ 加到运营商配置库。
- **ePDG 两次要求身份后直接 EAP-Failure（香港联通测试卡）** → 系统运营商配置也是 `carrier_wfc_ims_available_bool=false`，判断为该卡或套餐没开通 VoWiFi。

## 3. IMS / SIP

- **注册后发消息返回 494** → 非 REGISTER 请求也要带 `Security-Verify` 和 `Require / Proxy-Require: sec-agree`。
- **AKA 鉴权 SQN 不同步** → 用 AUTS 自动重新同步。
- **MVNO 配置不生效** → 带运营商名称的配置（CTExcel、CMLink UK）只在名称匹配时生效，普通 EE 卡回落到 `ee` 配置。
- **想用 UUID 作 `+sip.instance` 与基带 VoLTE 并存** → 注册能成功，但 EE 对该实例的**所有呼出**在 1 秒内回 487。对比测试：IMEI 实例（`urn:gsma:imei:…`）正常接通。→ 改回 IMEI 实例。
- **INVITE 收到 5xx 导致整个 UA 关闭、挂断所有通话** → 只有 REGISTER 的 408/5xx 才视为 P-CSCF 故障。
- **Globe 注册回 403 "Service not allowed in this location"** → SBC 按网络出口判断是否在本国。→ 不伪造位置信息；收到 403 后等 10 分钟再试，避免频繁打扰运营商。
- **「乱码短信」** → 其实是发给 SIM 卡的 OTA 数据下载（TP-PID 0x7F 或 UDH IEI 0x70/0x71）。App 无法把它交给 UICC。→ 不入收件箱，回 RP-ERROR cause 41（临时失败），让短信中心改由基带重发。
- **发短信被拒 RP cause 21、呼出 +86 号码被 487** → 原生拨号器和原生短信用同一张卡也一样失败，属于卡的账户限制。
- **3UK 收不到短信** → 网络没有把短信投到 IMS 注册上（推测走电路域），不是程序问题。

## 4. 接管系统 IMS（ImsService）

- **接管后功能列表为空、卡槽的运营商 IMS 被清空** → `cmd phone ims set-ims-service` 必须带 `-f`。
- **只接管 MMTEL 时来电两边都不响** → 打开系统 WLAN 通话后，高通 IMS 也自己注册了一份 VoWiFi，网络把来电送给它，而它在框架层已没有会话。→ 用 `-f 0,1` 同时接管 EMERGENCY_MMTEL 和 MMTEL；紧急呼叫仍不走本模块（`shouldProcessCall` 返回 CSFB）。
- **系统设置开关报 `ImsException: changeMmTelCapability`** → 本平台不会自动回调 `onFeatureReady()`。→ 创建 MMTEL 后 500 ms 主动 `setFeatureState(STATE_READY)`。
- **卡在 4G 时来电进不来，对方听「暂时无法接通」** → 基带在**内部**经 LTE 的 IMS 承载注册 VoLTE，与框架层由谁提供 IMS 无关，网络把来电送到基带，基带交不到系统。
  - 试过把 IMS 的 APN 改掉：AP 侧 IMS 连接确实断了，但基带自己维护的 IMS 承载照常在线，无效，已恢复。
  - 试过手动选网到测试网络 001-01：60 秒后基带报错并重新注册回网络，不可靠。
  - 试过关电台：会把另一张卡一起断电（日志可见 PHONE0 + PHONE1）。
  - 试过只允许 CDMA：基带直接拒绝（error 6）。
  - 可行：把这张卡限制为 2G（不含 LTE/NR），基带 VoLTE 下线。现在作为「测试功能」保留，推荐在系统设置里改首选网络类型。
- **系统拨号器提示「无法连接到移动网络」** → 卡没有蜂窝信号时，系统在拨号前就因服务状态为「无服务」拒绝了，请求根本到不了 ImsService。→ 注册成功时用 `setVoiceServiceStateOverride` 告诉系统语音在服务，未注册或交还卡槽时撤销。
- **网络回 180/183 时通话崩溃、无声并被取消** → `ImsStreamMediaProfile` 的 4 参数构造是隐藏 API，运行时 `NoSuchMethodError`；异常还让后面的音频启动被跳过。→ 改用公开的 5 参数构造；上报系统失败用 `runCatching` 包起来，不影响音频。**编译桩里有的方法不代表运行时能调。**
- **想强制显示运营商名** → 卡里没有 SPN 时名称为空。用户决定不强制，避免逐卡适配；只用系统从卡或网络得到的名称。

## 5. 系统界面与 ROM（ColorOS）

- **状态栏不显示 WiFi 通话图标**
  - 没连 WiFi 时：ColorOS 只在注册技术为 CROSS_SIM、且该卡「跨卡通话」打开时保留图标。→ 走移动数据时上报 CROSS_SIM，并打开该卡的跨卡通话设置（不伪造 WiFi）。
  - 系统 WLAN 通话开关关着时：注册技术为 IWLAN 也不显示。→ 现在系统开关就是模块的总开关，模块开着时系统开关一定是开的。
  - 收起的状态栏空间不够时，ColorOS 会把 VoLTE/VoWiFi 图标都隐藏，下拉通知栏能看到，不是本模块的问题。
- **网络切换时状态栏闪「无服务」** → 隧道重建期间立即向系统报告注销。→ 20 秒内重连成功就不报。
- **ColorOS 在后台或 App 更新时停掉前台服务** → App 改为常驻系统进程（`android:persistent`，需作为系统应用安装）。
- **系统设置里开关 WLAN 通话，模块不同步** → 卡槽交还原生后，系统不会再通知本模块。→ 常驻进程监听 SIM 数据库里的开关（加 60 秒兜底检查）。
- **ColorOS 分身用户里也启动了一份常驻 App，并不断崩溃** → 系统应用会被装进所有用户。→ 安装和开机时从非 0 用户删除，代码里只在主用户初始化。
- **WebUI 读频段为空** → 读服务状态时声明了放弃位置权限，系统把小区信息（频段、信道）抹掉了。→ `getServiceStateForSlot(slot, false, false, …)`。
- **拿不到运营商峰值速率** → 本机基带上报的默认承载 QoS 为空（`defaultQos=null`），系统只有基带自测的链路容量估计。→ 不显示速率。
- **WebUI 底栏随页面滑动（KsuWebUI）** → 改为 body 不滚动，内容区单独滚动，底栏作为 flex 子元素。

## 6. 模块打包、安装与升级

- **常驻 App 不能用 adb 更新** → `INSTALL_FAILED_INVALID_APK: Persistent apps are not updateable`。先 `pm uninstall-system-updates` 再装虽然能装上，但系统用已删除的旧路径重启常驻进程，`GraphicsEnvironment` 里 `getResources()` 为空，崩溃循环，只有重启能恢复。→ 正常更新方式：刷新模块并重启。
- **刷了新模块，跑的还是旧 App**
  - `/data` 里留着之前 adb 装的版本，版本号不比模块里的低时会盖住模块里的新 APK。→ **每次发布都提高 versionCode**；安装脚本执行 `pm uninstall-system-updates app.vowifi`。
  - KernelSU 叠加到 `/system` 的目录时间固定为 1971 年，系统认为缓存的解析结果仍有效。→ 安装脚本清空 `/data/system/package_cache`。
- **纯模块安装时 `libgojni.so not found`** → 系统应用不会解压 APK 里压缩存放的原生库。之前一直有 adb 装的版本盖在上面，所以没暴露。→ 原生库改为在 APK 里不压缩、16 KB 对齐存放（`useLegacyPackaging = false`，`extractNativeLibs=false`），直接从 APK 加载；模块包也从 11 MB 降到约 6.6 MB。
- **开机后第一次连接被 EE 拒绝** → 本模块比 NetProxy 先启动，先直连出去被按地区拒绝，之后按设计退避 60 秒。→ 「跟随系统」模式下装了 NetProxy 时，开机先等它就绪（最长 90 秒）。
- **WLAN 通话关了，来电却进不来** → 只停了 WiFi 通话，卡槽仍被接管，基带收到的来电交不到系统。→ 关闭时一并撤销系统改动并交还卡槽。

## 7. 让原厂 IMS 直接做海外卡 WiFi 通话（结论：在国内不可行）

试过不自建隧道、只放开原厂高通 IMS：
- 原厂会尝试把 IMS 切到 WiFi，IKE 由**基带**发起，经 `r_rmnet_data0` 转给系统再从 `wlan0` 发出；回包由硬件直接送回基带。
- 路由器只代理 TCP 时，UDP 从国内出去，EE 回 9003。手机上开着 NetProxy 也接不住这部分流量；就算硬改去程，回程也回不到基带。
- 走另一张卡流量的「跨卡通话」隧道完全在基带内部，更无法代理。
- 结论：只有当 WiFi 的 UDP 本身就从海外出去（例如整台路由器走海外隧道）时原厂才能用；在国内用按地区限制的海外卡，只能靠本模块自建隧道。

## 8. 排查习惯

- ColorOS 系统日志量很大，logcat 几分钟就会被冲掉。App 会把 IMS / Telephony 相关的系统日志持续写到 `files/logs/system.log`，排查优先看它和 `app.log`。
- 网络异常先查代理和 DNS（如 `/dev/netproxy/service.json`、`dumpsys dnsresolver`），再怀疑代码。
- 运营商侧的拒绝（9003、403、487、RP cause 21）先用原生拨号器 / 原生短信或换网络出口对比，确认是不是卡或地区的限制。
- 日志和对话里不要出现 IMSI、ICCID、手机号；需要时做掩码。
