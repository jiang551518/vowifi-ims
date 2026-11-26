# WiFi 通话 IMS 服务（vowifi_ims）

用软件实现 VoWiFi（WiFi 通话），并把它接入 Android 系统电话：系统拨号器、来电界面、通话记录、系统短信和设置里的「WLAN 通话」开关都能直接用，体验与原生 WiFi 通话一致。手机里不再有单独的 App 界面，管理全在 KernelSU 的模块 WebUI 里完成。

> ⚠ 目前只在 realme RMX6699（Android 16 / ColorOS / 骁龙，KernelSU）上验证可用。其他机型、系统和 root 方案均未测试。

---

## 1. 它解决什么问题

原生 WiFi 通话由基带和厂商的 IMS 服务（高通 `org.codeaurora.ims`）实现，有几个限制：

- 运营商没给这台手机或这个地区开放 WiFi 通话时，开关是灰的或者打开也注册不上。
- 隧道只能走当前连接的 WiFi，不能走另一张卡的流量或代理。
- 出了问题看不到原因。

本模块用自己的协议栈完成 VoWiFi 全流程：IKEv2 隧道、IMS 注册、通话、短信。它再以「系统 IMS 服务」的身份把这些能力交给 Android，所以系统界面全部照常工作，隧道可以走 WiFi、移动数据（包括另一张卡）或代理。

## 2. 整体结构

```
┌──────────────────────── Android 系统 ────────────────────────┐
│ 系统拨号器 / 来电界面 / 短信 App / 设置「WLAN 通话」          │
│                    │  Telephony / Telecom                    │
│                    ▼                                         │
│   ImsResolver ──绑定──► app.vowifi 的 ImsService             │
│   (卡槽 N 的 MMTEL + 紧急 MMTEL 被接管，不再给高通 IMS)       │
└────────────────────┬─────────────────────────────────────────┘
                     │ 拨号 / 接听 / 挂断 / DTMF / 短信 TPDU / 注册状态
┌────────────────────▼──────── app.vowifi（无界面，priv-app）──┐
│ ImsHub / VoWifiMmTel / VoWifiCallSession / VoWifiSms          │
│ Core（状态汇总）  VoWiFiService（前台服务，保持注册）         │
│ CallAudio（AudioRecord/AudioTrack + AMR 编解码）              │
│ WebBridge → status.json      ControlReceiver ← WebUI 命令     │
│ RootHelper（su 1000 运行：SIM 鉴权、APDU、telephony 特权调用）│
│ ┌──────────── Go 协议核心 libgojni.so ───────────────────┐   │
│ │ ike: IKEv2 + EAP-AKA/AKA' + 用户态 ESP（NAT-T 4500）    │   │
│ │ netio: gVisor 用户态 TCP/IP 栈（隧道内地址）            │   │
│ │ ims: SIP UA（注册 / sec-agree / 通话 / 短信 / RTP）     │   │
│ │ carrier: 运营商配置库（ePDG、APN、域名、P-CSCF 参数）   │   │
│ └─────────────────────────────────────────────────────────┘   │
└────────────────────┬─────────────────────────────────────────┘
                     │ UDP 4500（ESP-in-UDP）
                     ▼
     出口网络：WiFi / 移动数据（任一张卡）/ 系统代理
                     ▼
             运营商 ePDG ──► IMS 核心网（P-CSCF）
```

模块里的文件：

| 文件 | 作用 |
|---|---|
| `system/priv-app/VoWiFi/VoWiFi.apk` | 以系统特权应用身份安装的 `app.vowifi` |
| `system/etc/permissions/privapp-permissions-app.vowifi.xml` | 特权权限白名单（空） |
| `customize.sh` | 安装时创建 `/data/adb/vowifi/ims.conf`（App 记录当前接管的卡槽） |
| `service.sh` | 开机后确认 App 已运行；是否接管由 App 按系统开关决定 |
| `webroot/index.html` | KernelSU WebUI（状态 / 设置 / 日志） |
| `action.sh` | 模块管理器「操作」按钮，打印状态摘要 |
| `uninstall.sh` | 卸载时交还卡槽并清理 |

## 3. 建立 WiFi 通话的过程

首页的六步进度对应下面六个阶段。

### ① 解析 ePDG
按 SIM 的 PLMN（和运营商名，用于区分 MVNO）在运营商配置库里查 ePDG 地址。没有专门配置时按 3GPP TS 23.003 的标准域名 `epdg.epc.mncXXX.mccYYY.pub.3gppnetwork.org` 解析。DNS 可以选系统解析、DoH 或手填 IP。

### ② IKE_SA_INIT
向 ePDG 的 UDP 500/4500 发起 IKEv2（RFC 7296），协商加密套件和 DH 组。常见结果是 aes128-sha256-modp2048，老网络也支持 3DES/DH2。程序强制走 NAT-T（UDP 4500），这样在任何 NAT 或代理后面都能通。ePDG 进入防洪模式回 COOKIE 时，按规范原样重发，Nonce 和 DH 不变。

### ③ EAP-AKA / AKA' 鉴权
IKE_AUTH 里用 SIM 卡做 EAP-AKA（RFC 4187）或 EAP-AKA'（RFC 5448）：

1. 身份使用永久 NAI `0<IMSI>@nai.epc.mnc….3gppnetwork.org`。
2. ePDG 下发 RAND/AUTN。
3. **RootHelper** 以 `su 1000`（system 身份）调用 `TelephonyManager.getIccAuthentication`，让 SIM 卡计算 RES/CK/IK。
4. SQN 不同步时自动用 AUTS 重新同步。

密钥从不离开 SIM 卡，算法在卡里完成。

### ④ 隧道
鉴权通过后，ePDG 分配内层地址（通常只有 IPv6）和 P-CSCF 地址。ESP 数据面完全在用户态实现：加解密以后的 IP 包交给 **gVisor 用户态网络栈**。整条链路不需要 TUN 设备，不改系统路由，也不影响手机其他网络。

### ⑤ IMS 注册
在隧道内向 P-CSCF 发 SIP REGISTER（3GPP TS 24.229）：

- 用 IMS-AKA 摘要鉴权（再次由 SIM 卡计算），先按 ISIM（IMPI/IMPU/域名）注册，没有 ISIM 时由 IMSI 推导身份。
- 用 sec-agree（RFC 3329 / TS 33.203）建立 IPsec 传输模式安全关联（如 hmac-sha-1-96 + aes-cbc），后续 SIP 全部在这层保护里收发，同样在用户态完成。
- `+sip.instance` 使用 IMEI（`urn:gsma:imei`），与原生终端一致，部分运营商只接受这种形式。
- 注册后自动续订、TCP 保活。出错时（423、断线、408/5xx）先换另一个 P-CSCF 重试。

### ⑥ 就绪
注册成功后，引擎通知系统 IMS 服务上报「已注册」，系统开始把这张卡的通话和短信交给本模块。

断网或网络切换（WiFi ↔ 移动数据、飞行模式）时，守护循环会自动重建隧道和注册，退避间隔为 2 s、5 s、15 s、30 s、60 s。被网络明确拒绝时（例如 403 位置限制）等 10 分钟再试，避免频繁打扰运营商。

## 4. 如何接入系统电话

### 总开关：系统「WLAN 通话」开关
所选 SIM 卡在 设置 → 移动网络 → 卡信息 里的「WLAN 通话」开关就是模块的总开关：

- **打开**：App 把这张卡所在卡槽的系统 IMS 指向自己，然后建隧道、注册。
- **关闭**：App 注销、停止隧道，撤销跨卡通话设置、语音状态覆盖等改动，再把卡槽交还手机自带的 IMS，VoLTE 立即恢复原生。

WebUI 里的开关和通知栏的「关闭 WiFi 通话」按钮也只是去拨这个系统开关，所以几处永远一致；ColorOS 状态栏的 WiFi 通话图标也要求这个开关开着。

App 是常驻系统进程（`android:persistent`，由模块作为系统应用安装），开机自动运行，ColorOS 不会清理它。它一直监听 SIM 数据库里的这个开关（卡槽交还原生后，系统已不会通知本模块，只能靠自己监听），另外每 60 秒兜底检查一次。切换 eSIM Profile 时按新号码自己的开关决定。

### 接管卡槽的 IMS 服务
Android 的 `ImsResolver` 为每个卡槽绑定一个 ImsService。开关打开时 App 执行：

```sh
cmd phone ims set-ims-service -s <slot> -c -f 0,1 app.vowifi
```

`-f 0,1` 表示同时接管 EMERGENCY_MMTEL(0) 和 MMTEL(1)，高通 IMS 不再处理这个卡槽。否则高通会自己再注册一份 VoWiFi，网络会把来电送给它。这个覆盖不持久，重启后恢复，开机后由 App 按开关状态重新决定。当前接管的卡槽记在 `/data/adb/vowifi/ims.conf`，供卸载脚本使用。

紧急呼叫不走本模块：不声明紧急通话能力，紧急号码一律交回电路域（CSFB）。

### 通话
- **呼出**：系统拨号器 → Telephony 调用 `VoWifiCallSession.start` → 引擎发 INVITE（SDP 优先 AMR-WB，回落 AMR，支持 preconditions）。180/183/200 依次映射为 `callSessionProgressing / Initiated`，失败的 SIP 码映射为系统的 `ImsReasonInfo`。
- **呼入**：引擎收到 INVITE → `notifyIncomingCall` → 系统来电界面响铃，接听、拒接、挂断都在系统界面操作。
- **音频**：MMTEL 声明 `AUDIO_HANDLER_ANDROID`，Telecom 进入 VoIP 音频模式（听筒、扬声器、蓝牙路由都由系统负责）。App 里的 `CallAudio` 用 AudioRecord/AudioTrack + MediaCodec 做 AMR-WB/AMR 编解码，经 RTP（含抖动缓冲、RFC 4733 DTMF）收发。
- 通话记录、静音、DTMF 都是系统原生的。

### 短信
接管时引擎切换为「原始 PDU 模式」：

- 收到的 SMS-DELIVER 和状态报告原样交给系统短信栈（`ImsSmsImplBase.onSmsReceived`），系统确认后再回 RP-ACK。
- 系统要发的 TPDU 封装成 RP-DATA，经 SIP MESSAGE 发给短信中心。

短信由系统短信 App 收发和保存。发给 SIM 卡的 OTA 数据短信（TP-PID 0x7F）不能经此交给卡，所以回临时失败，让短信中心改由基带重发。

### 让系统认为「这张卡可以打电话」
隧道不经过平台自带的 IWLAN，系统的服务状态仍可能是「无服务」，系统拨号器会直接拒绝（提示「无法连接到移动网络」）。所以注册成功后：

- 用 Telecom 提供的 `ITelephony.setVoiceServiceStateOverride` 把这张卡的语音状态标为在服务；未注册或交还卡槽时撤销。
- 注册技术按出口如实上报：经 WiFi 报 **IWLAN**，经移动数据报 **CROSS_SIM**（跨卡/备用通话），同时打开该卡的跨卡通话设置。ColorOS 只在这两种情况下显示 WiFi 通话图标，系统不会把移动数据伪装成 WiFi。

运营商名只使用系统从 SIM 卡或网络得到的名称，模块不强制设置品牌名。

### 网络切换时不闪「无服务」
隧道因网络切换重建时，20 秒内重新注册成功就不告诉系统掉线，状态栏不会闪「无服务」；超过 20 秒才如实报告。这期间打进来的电话会接不到。

## 5. 出口网络

| 模式 | 行为 |
|---|---|
| 跟随系统 | 走默认网络，包括 VPN 和透明代理 |
| WiFi | 隧道绑定到 WiFi 网络 |
| 移动数据 | 隧道绑定到蜂窝网络，WiFi 在线时也会临时拉起蜂窝 |

另有两个辅助选项：

- **SOCKS5**：设置里可填 SOCKS5（UDP ASSOCIATE）。
- **绕过透明代理**：选 WiFi 或移动数据时，模块读取透明代理在 `ip rule` 里的放行标记，并在 iptables mangle 表里只给本应用的流量打上该标记，让隧道直连。停止服务或换回「跟随系统」时删除这条规则。

**代理只适配了 NetProxy**：[NetProxy](https://github.com/Fanju6/NetProxy-Magisk)（Fanju6/NetProxy-Magisk，基于 sing-box 内核的 Magisk/KernelSU 透明代理模块，测试版本 v8.7.0）。「跟随系统」时隧道经 NetProxy 出去，出口 IP 就是代理节点的 IP；「WiFi / 移动数据」时按上面的方式绕过 NetProxy 直连。其他透明代理（Clash、Box 类模块等）和 VPN 应用没有测试，绕过规则可能不生效。开机时 NetProxy 比本模块启动得晚：「跟随系统」模式下，装了 NetProxy 时模块会先等它就绪（最长 90 秒）再连接，避免先直连被运营商拒绝。

首页的出口 IP 和国家按当前出口模式检测（跟随系统时即 NetProxy 的出口），用于判断运营商看到的接入地。部分运营商会按地区拒绝接入：例如 EE 系的卡（CTExcel、CMLink UK）从国内 IP 接入会被拒（9003），需要经 NetProxy 走海外节点，且节点要支持 UDP。

## 6. 蜂窝网络与基带 VoLTE

**首选网络类型由系统设置管理**。WebUI 只显示两个卡槽各自的首选网络类型、当前网络（2G/3G/4G/5G NSA/5G SA、是否漫游）、频段与信道、载波带宽和信号（RSRP/RSRQ/SINR）；点卡片会打开系统里这张卡的设置页去修改。

同一号码如果基带 VoLTE 也在线，网络通常把来电送给基带。接管后基带的来电交不到系统，会表现为「对方听到暂时无法接通」。

**推荐：「开关 WiFi 通话时同步开关高清通话」**（WebUI「实验性功能」）。开启后：

- 打开 WiFi 通话时，模块在接管前（卡槽仍由手机自带 IMS 管理时）关掉这张卡的「高清通话」。基带随即撤下 4G 上的语音能力，只保留收发短信和补充业务的 IMS 注册；4G 信号和上网不受影响。然后模块接管，来电改走 WiFi 通话。
- 关闭 WiFi 通话时，模块先交还卡槽，再把高清通话打开，基带恢复 VoLTE。
- 只恢复模块自己关掉的卡（每张卡单独记录，重启后也有效），你原本就关着高清通话的卡不会被打开。切换 eSIM 后，原来那张卡即使已不在用，也会被恢复。
- 已在 3UK 和 CTExcel（EE）上验证：开着 4G 时来电走模块并响铃。基带在 4G 上保留的短信注册是否会收走短信，尚未验证。
- **注意：这张卡同时是上网卡时，不要开这个选项。** 实测 CTExcel 漫游在中国电信 4G 上、并设为默认上网卡时，高清通话一关，网络就以「不允许此网络的 EPS 服务」（EMM 原因 14）拒绝 4G 注册，整张卡无服务：没有流量，WiFi 通话也因为连不上网而失败。打开高清通话后约 25 秒恢复。同一张卡不作上网卡时、以及 3UK，关掉高清通话 4G 都正常。推测是漫游网络没有 2G/3G 回落接电话，作为上网卡注册 4G 时要求具备 4G 语音。

也可以在系统设置里把这张卡的首选网络类型改成不含 4G/5G 的类型。

WebUI 的「实验性功能」里还保留了「开启时将此卡切换为 2G」（慎用，一般被上面的选项取代）：WiFi 通话运行期间用系统界面看不到的 power 原因把这张卡锁在 2G，关闭时恢复；不改系统设置里的首选网络类型。代价是信号明显变差，没有 2G 网络的地方会完全无信号。

## 7. 管理界面（WebUI）

App 没有界面，整个管理界面就是 `webroot/index.html`，运行在 KernelSU 的 WebUI 里（Magisk 用户见第 9 节）：

- **读状态**：App 把引擎状态、SIM 列表、设置、出口 IP 等写入 `/data/data/app.vowifi/files/webui/status.json`（变化后约 250 ms 原子替换）。WebUI 每秒用 root 读取一次。
- **发命令**：WebUI 通过
  ```sh
  am broadcast -n app.vowifi/.control.ControlReceiver -a app.vowifi.CONTROL --es cmd <命令> [--es arg <参数>]
  ```
  控制 App，可用命令有 start / stop（拨系统 WLAN 通话开关）/ restart / sim / net / cell / radio / save / restore 等。接收器要求发送方持有 `MODIFY_PHONE_STATE`，只有 root、shell 和系统能调用，普通应用不能。
- **卡槽状态**：只读显示每个卡槽当前由本模块还是手机自带服务提供 IMS。
- **日志**：读取 `files/logs/app.log`，可按级别筛选。日志不记录 IMSI、ICCID 和手机号全文。

## 8. 恢复原生与卸载

只关掉 WiFi 通话**不等于**恢复原生：卡槽仍被接管，基带收到的来电交不到系统。所以：

- **关闭系统 WLAN 通话开关（或 WebUI 开关）**：模块停止引擎，撤销 2G 限制、跨卡通话设置、语音状态覆盖，再把卡槽交还手机自带的 IMS。
- **设置 → 恢复与卸载**：
  - 「恢复手机原生通话」：交还所有卡槽并暂停模块，系统开关不再启动它；在 WebUI 里重新打开 WiFi 通话即可恢复。
  - 「恢复原生并卸载模块」：在此基础上删除模块和 App，重启后完全回到原状。
- **直接在模块管理器里卸载**：`uninstall.sh` 在下次开机后交还卡槽，让 App 自行撤销系统改动，再删除 App 和 `/data/adb/vowifi`。

所有接管都不持久，即使模块失效，重启后系统也会回到自带的 IMS。

## 9. 在 Magisk 上使用

模块按 Magisk 标准格式打包（`META-INF`、`customize.sh`、`service.sh`、`uninstall.sh`、`system/` 叠加），Magisk、KernelSU、APatch 都能刷。开发和测试都在 KernelSU 上进行，**Magisk 未实测**，以下是按两者的差异整理的注意事项。

**安装**
1. 需要 Magisk v28 或更新版本（模块管理器里的「操作」按钮依赖 `action.sh`，v28 起支持）。
2. 在 **Magisk App → 模块 → 从本地安装** 里刷入 zip。不要在 Recovery 里刷：安装脚本要在系统运行时清理安装缓存、移除旧版 App，并把 App 从分身等其他用户里删除。
3. 重启手机。

**授予 root**

App 没有界面，第一次请求 root 时 Magisk 弹出的授权框容易错过或被自动拒绝。重启后进入 **Magisk → 超级用户**，确认「WiFi 通话」（`app.vowifi`）已允许；若被拒绝，打开开关后再重启一次。没有 root，模块无法读取 SIM、无法接管系统 IMS。

**使用**

- **总开关**：和 KernelSU 下一样，就是 设置 → 移动网络 → 卡信息 里所选 SIM 的「WLAN 通话」，不依赖任何管理界面。
- **管理界面**：Magisk 本身不能打开模块 WebUI，需要另装一个支持 KernelSU WebUI 接口（`ksu.exec`）的应用来打开 `webroot/index.html`，例如 [KsuWebUI](https://github.com/5ec1cff/KsuWebUIStandalone)、[MMRL](https://github.com/MMRLApp/MMRL) 或 [WebUI X](https://github.com/MMRLApp/WebUI-X-Portable)，并给它 root。不装也能用，只是看不到状态页、日志和高级设置。
- **状态摘要**：Magisk 模块列表里本模块的「操作」按钮会打印当前状态（是否接管、注册情况等）。

**卸载**

在 Magisk 模块列表里删除本模块并重启即可。卸载脚本会在开机后交还卡槽、让 App 撤销自己的系统改动，再删除 App 和 `/data/adb/vowifi`。

**可能遇到的问题**

- 升级模块后仍是旧版行为：先在 Magisk 里确认模块已更新，再重启一次。仍不行时，删除 `/data/system/package_cache/` 后重启，让系统重新读取模块里的 APK。
- 开机后一直不接管：多半是 root 没授权（见上），或系统里这张卡的「WLAN 通话」没打开。
- 遇到问题时，把 `/data/data/app.vowifi/files/logs/app.log` 和模块版本一起提交 Issue。

## 10. 已知限制

- 是否允许接入由运营商决定。部分运营商按接入地拒绝：Globe 在菲律宾以外回 403，EE 从国内直连回 9003 时需要走代理。也有的卡本身没开通 VoWiFi（如香港联通测试卡）。
- 部分卡有账户限制（如 CTExcel 不能发短信、不能打国际号码），用原生拨号同样失败，不是模块问题。
- 拨号后立即挂断、日志里是 `487` 且原因为 `CC_DIAM_CREDIT_LIMIT_REACHED`：运营商计费系统拒绝了通话，即**此卡余额不足**，充值后再拨。模块会弹出「通话失败：余额不足」通知并写入日志。
- 运营商若把短信走电路域（如 3UK），短信不会经 IMS 到达。
- 未连 WiFi、经移动数据通话时，ColorOS 收起的状态栏空间不足，不显示 WiFi 通话图标，下拉通知栏可见。没有蜂窝信号且卡里没有 SPN 时，运营商名为空。
- 不支持视频通话、通话保持、多方通话、RTT。
- 紧急呼叫始终走手机原生通道。

## 11. 标准与声明

协议实现依据公开标准独立编写：

- IETF：RFC 7296（IKEv2）、RFC 4187 / 5448（EAP-AKA / AKA'）、RFC 4303 / 3948（ESP / NAT-T）、RFC 3261 / 3329（SIP / sec-agree）、RFC 4867（AMR RTP 载荷）、RFC 4733（DTMF）。
- 3GPP：TS 24.302（非 3GPP 接入 / ePDG）、TS 33.402、TS 24.229（IMS SIP）、TS 33.203（IMS 接入安全）、TS 23.003（标识与域名）、TS 24.011 / 23.040（短信 RP / TP）。

没有使用任何第三方 VoWiFi 项目的代码。WebUI 和应用均为原创实现。作者：Claude Code & Lucas Jiang。
