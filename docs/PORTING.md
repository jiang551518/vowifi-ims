# 适配其他机型

本项目目前只在一台设备上验证过：**realme RMX6699（Android 16 / ColorOS / 高通骁龙，KernelSU）**。本文说明换到其他手机时，哪些部分与设备无关可以直接用，哪些需要逐项检查或修改，以及建议的验证顺序。

踩过的坑和原因分析见 [LESSONS.md](LESSONS.md)，工作原理见 [module/README.md](../module/README.md)。

---

## 1. 总览

| 层 | 代码位置 | 设备相关程度 |
|---|---|---|
| IKEv2 / EAP-AKA / ESP 隧道 | `core/ike` | 无关：只依赖标准协议和 SIM 鉴权结果 |
| 用户态网络栈 | `core/netio` | 无关 |
| IMS SIP（注册、通话、短信、RTP/AMR） | `core/ims` | 无关（运营商差异见第 4 节） |
| 运营商配置库 | `core/carrier` | 与机型无关，与运营商有关 |
| SIM 访问（AKA、APDU、读卡信息） | `core/sim` + `RootHelper.java` | **有关**：反射调用隐藏的 telephony 接口 |
| 系统 IMS 服务（MMTEL、短信） | `android/imsservice` | **有关**：@SystemApi 编译桩随系统版本变化；芯片厂商 IMS 的行为不同 |
| 状态栏、开关、语音状态 | `Core.kt`、`WfcSync.kt` | **有关**：依赖 ROM 的 SystemUI / Telephony 定制 |
| 通话音频 | `call/CallAudio.kt` | 基本无关（AOSP AudioRecord/AudioTrack/MediaCodec） |
| 模块安装与开机 | `module/*.sh` | **有关**：root 方案（KernelSU / Magisk / APatch）差异 |
| 代理绕过 | `service/ProxyBypass.kt` | **有关**：目前只认 NetProxy 的 sing-box 规则 |
| WebUI | `module/webroot` | 基本无关（需要能运行 KernelSU WebUI 接口的环境） |

---

## 2. 无需修改的部分

- **协议核心 `core/`**：IKEv2、EAP-AKA/AKA'、ESP、sec-agree、SIP、SDP、AMR、短信 RP/TP 编码全部按 3GPP / IETF 标准实现，不读取任何设备专有信息。单元测试 `go test ./ike ./ims ./esim ./carrier` 在电脑上就能跑。
- **调试 CLI `vwcli`**：不经过系统 IMS，可以在新机器上单独验证「SIM 鉴权 → 隧道 → IMS 注册 → 通话」整条链路（见第 5 节）。
- **ImsService 的主体逻辑**：`VoWifiMmTel`、`VoWifiCallSession`、`VoWifiSms`、`VoWifiRegistration` 只用 AOSP 的 `android.telephony.ims` @SystemApi，接口自 Android 12 起稳定。
- **接管命令**：`cmd phone ims set-ims-service / get-ims-service / clear-ims-service-override` 是 AOSP Telephony 自带的 shell 命令，覆盖不持久，重启即恢复。
- **通话音频**：Telecom 进入 VoIP 音频模式后由 App 录放音，听筒/扬声器/蓝牙路由交给系统。
- **常驻进程与分身用户处理**：`android:persistent` 是 AOSP 机制；安装脚本会把 App 从所有非 0 用户（ColorOS 分身 999、MIUI 双开、三星安全文件夹等）里删除，代码里也只在主用户初始化。

---

## 3. 需要逐项检查或适配的部分

### 3.1 CPU 架构
- Go 库只编译了 **arm64**（`gomobile bind -target=android/arm64`）。32 位设备需改为 `-target=android/arm` 或同时编两种。

### 3.2 Android 版本
- **最低 Android 12**（`minSdk 31`），只在 Android 16 上测过。
- **ImsService 编译桩**：`android/imsservice/libs/ims-stubs.jar` 是从**目标手机**的 `framework.jar` 生成的（`tools/gen_ims_stubs.py`）。换系统版本时重新生成：
  ```bash
  AJ=$ANDROID_HOME/platforms/android-37.0/android.jar
  adb pull /system/framework/framework.jar
  unzip framework.jar 'classes*.dex'
  for f in classes*.dex; do $ANDROID_HOME/build-tools/36.1.0/dexdump -d "$f" > "${f%.dex}.txt"; done
  python3 tools/gen_ims_stubs.py stubs-src "$AJ" classes*.txt      # 生成 Java 源码
  mkdir stubs-out
  javac -nowarn -source 8 -target 8 -bootclasspath "$AJ" -d stubs-out $(find stubs-src -name '*.java')
  jar cf android/imsservice/libs/ims-stubs.jar -C stubs-out .
  ```
  javac 会对 `android/telephony/ims/compat/feature/` 下两个用不到的旧类报 2 个错误，但其余类照常输出（本机为 230 个 class），这是预期的；不要把这两个类从编译里去掉，其他类依赖它们的声明。
- **隐藏重载**：编译桩里有的方法，运行时不一定能调。新增 @SystemApi 调用时，要确认用的不是 `@hide` 的重载（例：`ImsStreamMediaProfile` 的 4 参数构造是隐藏的，会在 180/183 时抛 `NoSuchMethodError`）。
- **反射调用的隐藏接口**（`RootHelper.java`）：以下方法名和参数在不同版本 / ROM 上可能变化，需逐个确认：

  | 用途 | 服务 | 方法 |
  |---|---|---|
  | SIM 鉴权（AKA） | `iphonesubinfo` | `getIccSimChallengeResponse` |
  | 读 SIM 列表 | `isub` | `getActiveSubscriptionInfoList` |
  | APDU 逻辑通道（ISIM / eSIM） | `phone` | `iccOpenLogicalChannel`（参数为 `IccLogicalChannelRequest`）、`iccTransmitApduLogicalChannel(ByPort)`、`iccCloseLogicalChannel`、`getUiccCardsInfo` |
  | 系统 WLAN 通话开关 | `phone` | `isVoWiFiSettingEnabled` / `setVoWiFiSettingEnabled` |
  | 语音在服务覆盖 | `phone` | `setVoiceServiceStateOverride` |
  | 跨卡通话设置 | `phone` | `isCrossSimCallingEnabledByUser` / `setCrossSimCallingEnabled` |
  | 网络类型（2G 实验开关） | `phone` | `get/setAllowedNetworkTypesForReason` |
  | 小区信息（WebUI） | `phone` | `getServiceStateForSlot(slot, false, false, pkg, null)`、`getSignalStrength` |

  在新手机上可用 `vwcli methods <服务> <方法名关键字>`（如 `vwcli methods phone VoWiFi`）和 `vwcli classinfo <类名>` 列出真实签名，再用 `vwcli invoke <服务> <方法> <JSON 参数数组>` 试调。

### 3.3 芯片厂商的 IMS（最关键）
接管前先看这张卡槽原来由谁提供 IMS：
```bash
cmd phone ims get-ims-service -s <slot>      # 设备级
cmd phone ims get-ims-service -s <slot> -c   # 运营商级
```

- **高通（已验证）**：设备 IMS 是 `org.codeaurora.ims`。必须用 `-f 0,1` 同时接管 EMERGENCY_MMTEL 和 MMTEL：只接管 MMTEL 时，打开系统 WLAN 通话会让高通自己再注册一份 VoWiFi，网络把来电送给它，两边都不响。
- **联发科 / 三星 Exynos / 谷歌 Tensor（未测试）**：设备 IMS 包名、对 `-f` 功能列表的处理、是否回调 `onFeatureReady()` 都可能不同。本机平台不会自动回调，所以 `VoWifiImsService` 在创建 MMTEL 后主动 `markReady()`，其他平台需确认这样做没有副作用。
- **基带自带 VoLTE 抢来电**：框架层接管只换掉了系统 IMS 服务，基带仍会经 LTE 的 IMS 承载自己注册 VoLTE。同号双注册时网络通常把来电送给基带，而基带交不到系统，来电方会听到「暂时无法接通」。这在所有平台上都要验证：卡在 4G 时能否收到经本模块的来电。缓解办法是在系统设置里把这张卡的首选网络类型设为不含 4G/5G，或使用 WebUI 的「切换为 2G」实验开关。
- **紧急呼叫**：本模块不声明紧急通话能力，`shouldProcessCall` 对紧急号码返回 CSFB。换平台后务必确认紧急号码仍走基带。

### 3.4 ROM（系统界面与 Telephony 定制）
- **状态栏 WiFi 通话图标**：ColorOS 的 SystemUI 只在两种情况下显示：连着 WiFi 且注册技术为 IWLAN、系统 WLAN 通话开关打开；或者注册技术为 CROSS_SIM 且该卡「跨卡通话」打开。本模块据此在走移动数据时上报 CROSS_SIM，并打开跨卡通话设置。其他 ROM（HyperOS、One UI、原生 Pixel）的判断条件需反编译 SystemUI 确认，必要时调整 `Core.imsOverCellular()` 和 `ImsHub.onRegistration`。
- **「无法连接到移动网络」**：卡没有蜂窝信号时，TelephonyConnectionService 会因 `ServiceState` 为 OUT_OF_SERVICE 直接拒绝拨号。本模块用 `setVoiceServiceStateOverride` 解决，依赖 AOSP 的 `GsmCdmaPhone.mergeVoiceServiceStates` 逻辑，其他 ROM 需验证。
- **首选网络类型**：WebUI 读取 `Settings.Global preferred_network_mode<subId>`，这是 AOSP 的存法；部分 ROM 另有自己的数据库，显示可能不准（只影响显示）。
- **跳转系统 SIM 设置页**：用 `android.settings.NETWORK_OPERATOR_SETTINGS`，在 ColorOS 上解析到 `OplusSimSettingsImpl`。可用 `cmd package resolve-activity -a android.settings.NETWORK_OPERATOR_SETTINGS` 检查。
- **后台管控**：常驻系统进程不受一般的后台清理影响；如果某 ROM 仍会杀掉它，检查 `dumpsys activity processes` 里的 oom_adj。

### 3.5 Root 方案
- **KernelSU（已验证）**：模块目录叠加到 `/system` 后，目录时间固定为 1971 年，系统的安装缓存不会自动失效。安装脚本会清 `/data/system/package_cache` 并移除 `/data` 里的旧版 App。
- **Magisk / APatch（未测试）**：模块格式通用；Magisk 没有 WebUI，需另装 KsuWebUI / MMRL / WebUI X；root 授权弹窗容易错过（见模块 README 第 9 节）。
- App 找 su 的路径：`/system/bin/su`、`/system/xbin/su`、`/data/adb/ksu/bin/su`、`/debug_ramdisk/su`、`/sbin/su`、`/su/bin/su`（`core/sim/helper.go` 的 `findSU`）。RootHelper 以 `su 1000`（system 身份）运行，要求 su 支持切换到指定 uid。

### 3.6 代理
- `ProxyBypass.kt` 从 `ip rule` 里解析 sing-box 的放行标记（NetProxy 当前为 `fwmark 0x200000/0xe00000 goto`），在 mangle 表按本 App 的 uid 打标记。只适配了 [NetProxy](https://github.com/Fanju6/NetProxy-Magisk)；Clash / Box 类模块或 VPN 应用需按它们的规则改写。
- 开机等待代理就绪（`WfcSync.waitForNetProxy`）读取的是 `/dev/netproxy/service.json`，换代理软件时需要改成对应的就绪判断。

---

## 4. 与机型无关、但与运营商有关的部分

- **运营商配置库 `core/carrier`**：ePDG 地址、IMS 域、P-CSCF、sec-agree、SIP 传输等。标准 ePDG 域名解析不到时（如 Globe 实际用 `weconnect.globe.com.ph`）要手动加。
- **接入地区限制**：EE 系的卡（CTExcel、CMLink UK）从国内 IP 接入会在 EAP 之后回 9003（AUTHORIZATION_REJECTED），Globe 在 IMS 注册时回 403 "Service not allowed in this location"。这类卡必须经海外出口，且出口要支持 UDP。
- **未开通 VoWiFi 的卡**：如香港联通测试卡，ePDG 在要求两次身份后直接 EAP-Failure，系统运营商配置里也是 `carrier_wfc_ims_available_bool=false`。这不是机型问题。

---

## 5. 建议的验证顺序

1. **协议链路（不动系统）**：编译并推送调试工具，先确认 SIM 鉴权和隧道在新机器上可用。
   ```bash
   tools/dev.sh                                    # 需要 tools/device.local 里写好 ANDROID_SERIAL
   adb shell su -c '"/data/local/tmp/vwcli subs"'           # 找到 subId
   adb shell su -c '"/data/local/tmp/vwcli connect <subId>"'
   adb shell su -c '"/data/local/tmp/vwcli ims <subId> -hold 60s"'
   adb shell su -c '"/data/local/tmp/vwcli call <subId> -dial <测试号码>"'   # 会产生话费，谨慎
   ```
2. **隐藏接口**：`vwcli methods phone VoWiFi`、`vwcli classinfo android.telephony.ims.ImsStreamMediaProfile` 等，对照 3.2 节的表格确认签名。
3. **编译桩**：按 3.2 节用目标机的 `framework.jar` 重新生成后再编译 APK。
4. **记录原始状态**：`cmd phone ims get-ims-service -s <slot>`（带和不带 `-c`）、`dumpsys carrier_config` 中的 `carrier_wfc_ims_available_bool`、`getprop gsm.ims.type<slot>`。
5. **刷模块、打开系统 WLAN 通话**，依次验证：
   - 注册：`dumpsys telephony.registry` 里语音在服务；`getprop gsm.ims.type<slot>` 为 `vowifi`。
   - 系统拨号器呼出、来电（含卡在 4G 时）、系统短信收发。
   - 状态栏图标（连 WiFi 和不连 WiFi 两种）。
   - 关闭系统开关后卡槽回到原厂 IMS，VoLTE 恢复。
   - 重启后自动接管、分身用户里没有本 App 的进程。
6. **出问题先看日志**：`/data/data/app.vowifi/files/logs/app.log`（App 日志）、`system.log`（IMS / Telephony 相关系统日志，App 自动抓取）、`go_crash.log`（Go 引擎崩溃）。

---

## 6. 恢复原状

接管不持久，任何时候都可以：
```bash
cmd phone ims clear-ims-service-override -s <slot>
```
或直接重启手机。删除模块后重启，卸载脚本会交还卡槽、撤销 App 的系统改动并删除 App。
