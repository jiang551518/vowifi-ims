# vowifi-ims

在 root 过的 Android 手机上，用软件实现 VoWiFi（WiFi 通话），并作为系统 IMS 服务接入 Android 电话：系统拨号器、来电界面、通话记录、系统短信和「WLAN 通话」开关都能直接使用。隧道可以经 WiFi、任一张卡的移动数据或代理建立。发布形式是 KernelSU / Magisk 模块，管理界面是 KernelSU WebUI。

工作原理见 [module/README.md](module/README.md)。

> ⚠ 目前仅在 realme RMX6699（Android 16 / ColorOS / 骁龙，KernelSU）上验证可用。其他机型、系统和 root 方案均未测试，请自行评估风险。紧急呼叫始终走手机原生通道。

## 安装

1. 从 [Releases](https://github.com/jiang551518/vowifi-ims/releases) 下载 `vowifi-ims-vX.Y.Z.zip`。
2. **KernelSU / APatch**：在管理器的「模块」里安装后重启。**Magisk**（v28+，未实测）：在 Magisk App 里安装后重启，并到「超级用户」确认已给「WiFi 通话」授权，详见 [module/README.md 第 9 节](module/README.md#9-在-magisk-上使用)。
3. 在 设置 → 移动网络 → 卡信息 打开要用的 SIM 的「WLAN 通话」，模块即接管这张卡并连接；关闭即恢复原生通话。
4. KernelSU 中点本模块可打开管理界面（Magisk 需另装 KsuWebUI / MMRL / WebUI X）。代理目前只适配 [NetProxy](https://github.com/Fanju6/NetProxy-Magisk)。

## 目录

```
core/      Go 协议核心：IKEv2 + EAP-AKA/AKA'、用户态 ESP、gVisor 网络栈、
           IMS SIP UA（注册 / sec-agree / 通话 / 短信）、运营商配置库
android/   app：无界面的系统组件（前台服务、通话音频、RootHelper、WebUI 桥）
           imsservice：系统 ImsService（MMTEL + 短信）
module/    KernelSU / Magisk 模块脚本和 WebUI（webroot/index.html）
tools/     构建与调试脚本
```

## 构建

需要 Go、gomobile、Android SDK（compileSdk 37）和 NDK。

```bash
# 单元测试
cd core && go vet ./... && go test ./ike ./ims ./esim ./carrier

# Go 核心 → AAR
cd core && gomobile bind -target=android/arm64 -androidapi 26 \
  -javapkg app.vowifi.core -ldflags="-s -w" \
  -o ../android/app/libs/vowificore.aar ./mobile

# ImsService 编译桩（@SystemApi）：从手机的 framework.jar 生成，见 tools/gen_ims_stubs.py
#   输出到 android/imsservice/libs/ims-stubs.jar

# APK 和模块
cd android && ./gradlew assembleDebug
tools/build_module.sh   # → dist/vowifi-ims-module.zip
```

## 标准与声明

协议栈依据 3GPP / IETF / GSMA 公开标准独立编写（TS 24.302、TS 24.229、TS 33.203、RFC 7296、RFC 4187/5448、RFC 3329 等），没有复制任何第三方 VoWiFi 项目的代码。依赖库：[gVisor](https://github.com/google/gvisor)（Apache-2.0）、[euicc-go](https://github.com/damonto/euicc-go)（MIT）、golang.org/x 系列。

本项目仅供学习研究。使用者需遵守所在地法律和运营商的服务条款。

## 许可证

[GPL-3.0](LICENSE) © Claude Code & Lucas Jiang
