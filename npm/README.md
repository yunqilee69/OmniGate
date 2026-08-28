# @cloudomni/omnigate

[OmniGate](https://github.com/yunqilee69/OmniGate) 的 npm 发行包 —— OpenAI 兼容的本地 AI 网关,单二进制、零外部依赖。

> ⚠️ 本包**不内嵌 Go 二进制**。安装时(`postinstall`)会从 GitHub Releases 下载对应平台的二进制,因此安装期需要联网。

## 安装

```bash
npm install -g @cloudomni/omnigate
omnigate
```

首次运行会在 `~/.omnigate/` 下自动创建数据目录(SQLite、配置、日志),浏览器打开 <http://127.0.0.1:17777> 进入内嵌管理台。

## 安装时发生了什么

`postinstall` 会依次:

1. 识别操作系统与 CPU 架构
2. **双源下载**(自动回退,无需配置)：
   - 优先从 **Cloudflare R2 CDN**(中国大陆快,`cdn.yunke.icu`)下载
   - 失败则回退 **GitHub Release**(全球可用)
3. 下载对应平台的 `omnigate-<版本>-<os>-<arch>.tar.gz` 与 `omnigate_<版本>_checksums.txt`
4. 校验 tarball 的 sha256(不匹配或缺少条目则中止安装——fail closed)
5. 解压到 `node_modules/@cloudomni/omnigate/vendor/`
6. 为二进制添加可执行权限

## 使用

```bash
omnigate                        # 默认启动(数据目录 ~/.omnigate/)
omnigate --listen 0.0.0.0:8080  # 自定义监听地址
omnigate --db ~/work/og.db      # 覆盖 db 路径(支持 ~ 展开)
omnigate --log stdout           # 仅输出到 stdout
```

所有命令行参数原样透传给底层 Go 二进制。完整功能(加权路由、密钥轮询、阶梯熔断、统计、管理台)见[主项目 README](https://github.com/yunqilee69/OmniGate)。

## 支持平台

| 系统 | 架构 |
|---|---|
| Linux | amd64、arm64 |
| macOS | amd64(Intel)、arm64(Apple Silicon) |
| Windows | amd64、arm64 |

## 卸载

```bash
npm uninstall -g @cloudomni/omnigate
rm -rf ~/.omnigate   # 可选:一并清理运行数据
```

## 故障排查

**大多数情况下直接 `npm install` 即可**——双源策略(Cloudflare R2 + GitHub)+ 自动重试已覆盖常见网络问题。

**仍失败 / 极端受限网络**

使用 GitHub 加速镜像(仅作为第三层回退，R2 和 GitHub 都失败时)：

```bash
OMNIGATE_GH_PROXY=https://ghproxy.com npm install -g @cloudomni/omnigate
# 或 ghfast.top / gh-proxy.com 等其他镜像服务
```

放宽空闲检测窗口(秒,默认 30，仅在极慢网络下需要)：

```bash
OMNIGATE_DOWNLOAD_TIMEOUT=120 npm install -g @cloudomni/omnigate
```

**手动下载(离线环境 / 企业内网)**

```bash
# 1. 从 cdn.yunke.icu/v版本号/ 或 GitHub Release 页面手动下载对应平台 tarball
# 2. 解压出 vendor/omnigate(Windows 为 vendor/omnigate.exe)后:
OMNIGATE_SKIP_POSTINSTALL=1 npm install -g @cloudomni/omnigate
```

**跳过 postinstall(CI / 测试)**

```bash
OMNIGATE_SKIP_POSTINSTALL=1 npm install ...
```

**macOS Gatekeeper 拦截未签名二进制**

```bash
xattr -d com.apple.quarantine "$(npm root -g)/@cloudomni/omnigate/vendor/omnigate"
```

## 相关链接

- 主项目:<https://github.com/yunqilee69/OmniGate>
- 发布产物:<https://github.com/yunqilee69/OmniGate/releases>
- 协议:[MIT](./LICENSE)
