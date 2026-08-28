# 发布与分发

> **关键事实:** 最终交付物是 **Go 单二进制**,用户无需任何运行时依赖(Node、Python、数据库服务器都不需要)。
>
> 我们同时把**这个单二进制**通过 npm 包二次分发:`@cloudomni/omnigate` 在 `postinstall` 阶段按平台拉对应二进制到 `node_modules/@cloudomni/omnigate/vendor/`,然后 bin shim exec 出去。
>
> 即用户面对的是 npm 命令,但底层是 Go 二进制,**没有任何 Node 运行时要求**(安装期除外)。

---

## 1. 交付物形态

| 形态 | 说明 | 面向 |
|---|---|---|
| **npm 包 `@cloudomni/omnigate`** | postinstall 从 GitHub Releases 拉匹配的 Go 二进制到 `vendor/`,bin shim exec | 99% 的用户,一行命令 |
| **GitHub Release tar.gz** | 6 平台 × tar.gz + sha256 校验和 | 不想装 npm / 自动化脚本 / 服务器 |

**不发布的内容**:
- ~~Docker 镜像~~(用户用 npm 或本地 tar.gz 即可,无需镜像层)
- ~~go install~~(npm 包覆盖 Go 用户的安装路径,不需要再独立一个)
- ~~Homebrew tap~~(macOS 用户直接 `npm i -g`)

---

## 2. 数据布局(运行期)

```
~/.omnigate/                          # 用户主目录下集中管理
├── omnigate.db                       # SQLite
├── omnigate.db-wal / .db-shm         # WAL 副产物
├── config.yaml                       # 启动层配置(自动生成)
└── omnigate.log                      # 结构化日志
```

- 三个路径都可被 `--db` / `--config` / `--log` 覆盖,也接受 `~` 展开
- 首次运行:目录不存在则自动创建;`config.yaml` 缺失则用模板初始化
- 卸载:`npm uninstall -g @cloudomni/omnigate` + `rm -rf ~/.omnigate`(可选)
- 开发模式 `./start.sh` 用仓库内 `./data/`、`./config.yaml`、`./logs/backend.log`,与生产路径隔离

---

## 3. 版本策略

遵循 [SemVer 2.0.0](https://semver.org/lang/zh-CN/),格式 `vMAJOR.MINOR.PATCH`:

| 段 | 触发条件 |
|---|---|
| **MAJOR** | 不兼容的配置 / 数据 schema 变更,或核心架构重写 |
| **MINOR** | 向后兼容的新功能(MCP 网关、新协议适配、新统计维度) |
| **PATCH** | 向后兼容的 bug 修复 |

**预发布标签**(可选):
- `v1.1.0-rc.1` / `v1.1.0-beta.2` — 预发布,npm 默认不安装(`@latest` 仍指向上一 stable)
- `v1.1.0` — 正式版

**当前版本线:** `v0.1.0` (v1.0 设计已定稿,但生产实战前先以 0.x 迭代几个补丁)

---

## 4. 发布流程

### 4.1 准备发版

```bash
# 1. 切到 main 分支并拉新
git checkout main && git pull

# 2. 跑一遍测试
go test ./...

# 3. 确认 web 产物是最新的(开发期改动 web 必须重 build)
(cd web && npm ci --no-audit --no-fund && npm run build)

# 4. 检查 git 状态(应该没有 webui/dist 的改动;若有 revert)
git status
```

### 4.2 打 tag 触发自动发布

```bash
# 打 tag(annotated tag 携带发版说明)
git tag -a v0.1.0 -m "v0.1.0: initial public release"

# 推 tag,触发 .github/workflows/release.yaml
git push origin v0.1.0
```

CI 会自动(`.github/workflows/release.yaml`,三个 job 串行):

1. **test** — ubuntu / macOS / Windows 三平台矩阵跑 `go vet ./...` + `go test ./... -count=1`,外加 web 端 `tsc --noEmit` 类型检查(vite build 本身不查类型)
2. **release** — `goreleaser` 交叉编译 6 目标(3 平台 × amd64/arm64)+ 打包 + 生成 changelog,创建 GitHub Release 并上传 tar.gz + sha256 校验和;发布前先跑一次 snapshot dry-run 校验
3. **npm-publish** — 把 `npm/package.json` 版本号同步为 tag 版本后发布到 npm;`-rc`/`-beta` 等预发布 tag 发到 `@next` dist-tag(`@latest` 保持 stable)。认证走 **Trusted Publishing(OIDC)**,无需任何令牌

**前置配置(一次性,npmjs.com 网页操作):**

npm 发布不用 secret。取而代之,需在 npmjs.com 上把 GitHub 仓库配置为该包的信任发布方:

1. 登录 npmjs.com → 包 `@cloudomni/omnigate` → **Settings** → **Trusted Publisher** → 选 **GitHub Actions**
2. 填写:**Organization or user** `yunqilee69`;**Repository** `OmniGate`;**Workflow filename** `release.yaml`(仅文件名,大小写敏感);Environment 留空;**Allowed actions** 勾 `npm publish`
3. 保存即生效。之后 `release.yaml` 的 npm-publish job 凭 workflow 级 `id-token: write` 权限自动换取短期发布凭证(要求 npm CLI ≥11.5.1 / Node ≥22.14,job 已用 Node 24)

> 背景:npm 自 2026 年起逐步禁用"绕过 2FA 的令牌"直接发布(2027-01 全面生效),官方推荐的 CI 发布方式即 Trusted Publishing。本包 v0.1.0 即因旧式 Automation 令牌被 registry 拒绝,已于 2026-08 迁移至此方案。

**顺序约束:** goreleaser 必须先完成(把二进制推到 GitHub Releases),npm publish 再跑;否则用户装 npm 包时 postinstall 拉不到对应版本二进制。workflow 里用 `needs: release` 保证。

### 4.3 验证发布

```bash
# npm 主渠道
npm install -g @cloudomni/omnigate
omnigate --version
omnigate       # 看 ~/.omnigate/ 自动生成与否

# 直接下载校验
curl -L https://github.com/yunqilee69/OmniGate/releases/latest/download/omnigate-linux-amd64.tar.gz -o /tmp/og.tar.gz
tar tzf /tmp/og.tar.gz | head
sha256sum -c omnigate_0.1.0_checksums.txt
```

---

## 5. 平台矩阵

`goreleaser` 当前覆盖 6 个目标:

| OS | Arch | 备注 |
|---|---|---|
| Linux | amd64 | 主力 x86 服务器 |
| Linux | arm64 | Apple Silicon Linux 服务器、ARM 嵌入式 |
| macOS | amd64 | Intel Mac |
| macOS | arm64 | Apple Silicon |
| Windows | amd64 | x86 工作站 |
| Windows | arm64 | Windows on ARM |

未来按需追加:FreeBSD amd64、Linux 386。

---

## 6. 工具链

| 工具 | 用途 |
|---|---|
| [goreleaser](https://goreleaser.com/) | 交叉编译、打包、生成 changelog、创建 GitHub Release |
| [GitHub Actions](https://github.com/features/actions) | CI/CD(tag 触发 release workflow) |
| [npm registry](https://www.npmjs.com/) | `@cloudomni/omnigate` 包发布 |
| Node ≥ 18 | **仅在安装 npm 包时需要**,运行期不需要 |

**不依赖**:CGO、Docker、Go 工具链(终端用户)、`goproxy.io` 镜像、独立前端 CDN。

---

## 7. 发布配置落点

| 文件 | 作用 |
|---|---|
| `.goreleaser.yaml` | 构建矩阵、打包格式、Release notes 模板 |
| `.github/workflows/release.yaml` | tag push 触发:goreleaser + npm publish |
| `npm/` | npm 包源(package.json / bin / postinstall) |
| `docs/RELEASE.md` | 本文件,流程说明 |
| `.github/workflows/ci.yaml` *(可选,见 §7)* | 每个 PR 自动跑测试 + vet + build |
| `docs/RELEASE.md` | 本文件,流程说明 |

---

## 8. 建议补的 CI(未提交,可按需开启)

`ci.yaml` —— 每个 PR 自动跑 `go test` + `go vet` + `go build`,防止主分支 broken:

```yaml
name: ci
on:
  pull_request:
    branches: [main]
  push:
    branches: [main]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - run: go vet ./...
      - run: go test ./... -count=1
      - run: go build ./cmd/omnigate
```

加上前确保 `.github/workflows/` 目录已建。

---

## 9. FAQ

**Q: 为什么不发 Docker 镜像?**
A: 项目定位是"本地工具 + 单一可执行文件",Docker 反而多一层间接(npm 或 tar.gz 解压一行就跑)。如果将来需要 Kubernetes 化,再讨论加 distroless 镜像。

**Q: npm 包体积多大?**
A: 包本体仅 ~10 KB(只含 Node shim + postinstall 脚本 + README)。真正的 Go 二进制在用户安装时从 GitHub Releases 下载到 `node_modules/@cloudomni/omnigate/vendor/`,约 34 MB。所以 `npm i -g` 体感是"瞬间装好",但首次 `omnigate` 启动前会触发一次下载(几十秒,看网速)。

**Q: 用户升级怎么处理?**
A:
- npm 路径:`npm update -g @cloudomni/omnigate` 会触发 postinstall 下载新版二进制覆盖 `vendor/`,`~/.omnigate/` 数据目录不动,SQLite 由 GORM 自动迁移。
- tar.gz 路径:下载新版 tar.gz 解压,替换旧二进制;`~/.omnigate/` 同样不动。

**Q: macOS Apple Silicon 用户会遇到签名问题吗?**
A: 我们用 `CGO_ENABLED=0` 静态编译,二进制天然通过 `xcrun` Gatekeeper 的"未签名"提示(首次运行需右键打开或 `xattr -d com.apple.quarantine /path/to/omnigate`)。如有大量反馈再加正式签名。
