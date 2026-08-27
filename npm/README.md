# @cloudomni/omnigate

npm distribution wrapper for [OmniGate](https://github.com/cloudomni/omnigate) — an OpenAI-compatible AI gateway.

> ⚠️ This package **does not** ship the Go binary. It downloads the matching binary from GitHub Releases on `postinstall`. Network access is required at install time.

## Install

```bash
npm install -g @cloudomni/omnigate
omnigate
```

`postinstall` will:

1. Detect your OS / architecture
2. Download the matching `omnigate-<version>-<os>-<arch>.tar.gz` from the [release page](https://github.com/cloudomni/omnigate/releases/tag/v`npm view @cloudomni/omnigate version`)
3. Extract it into `node_modules/@cloudomni/omnigate/vendor/`
4. Mark the binary executable

On first run, OmniGate will create `~/.omnigate/` (db, config, log) automatically.

## Usage

```bash
omnigate                      # start with defaults (~/.omnigate/)
omnigate --listen 0.0.0.0:8080 # custom listen address
omnigate --db ~/work/og.db    # override db path (tilde expands)
omnigate --log stdout         # log to stdout only
```

All flags are forwarded to the underlying Go binary verbatim.

## Uninstall

```bash
npm uninstall -g @cloudomni/omnigate
rm -rf ~/.omnigate   # optional: wipe runtime data
```

## Supported platforms

| OS | Arch |
|---|---|
| Linux | amd64, arm64 |
| macOS | amd64 (Intel), arm64 (Apple Silicon) |
| Windows | amd64, arm64 |

## Troubleshooting

**postinstall download fails (firewall / offline)**

```bash
# 1. Manually download the tarball from the release page
# 2. Extract it so that `vendor/omnigate` (or `vendor/omnigate.exe`) exists
OMNIGATE_SKIP_POSTINSTALL=1 npm install -g @cloudomni/omnigate
```

**Skip postinstall entirely (e.g. in CI)**

```bash
OMNIGATE_SKIP_POSTINSTALL=1 npm install ...
```

**macOS Gatekeeper blocks the binary**

The Go binary is unsigned. First run:

```bash
xattr -d com.apple.quarantine "$(npm root -g)/@cloudomni/omnigate/vendor/omnigate"
```

## See also

- Main project: <https://github.com/cloudomni/omnigate>
- Release artifacts: <https://github.com/cloudomni/omnigate/releases>
- License: [MIT](./LICENSE)
