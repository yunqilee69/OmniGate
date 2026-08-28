#!/usr/bin/env node
// postinstall: 按当前 OS/arch 从 GitHub Releases 拉对应 tar.gz,
// 校验 sha256 后解压到 vendor/,chmod +x(类 unix)。包版本号必须与 GitHub Release tag 一致。

const https = require('https');
const crypto = require('crypto');
const fs = require('fs');
const path = require('path');
const tar = require('tar');

const VERSION = require('../package.json').version;

// 用户可设置 OMNIGATE_SKIP_POSTINSTALL=1 跳过(比如 CI 测试场景)。
if (process.env.OMNIGATE_SKIP_POSTINSTALL) {
  console.log('[omnigate] postinstall skipped (OMNIGATE_SKIP_POSTINSTALL set)');
  process.exit(0);
}

const archMap = { x64: 'amd64', arm64: 'arm64' };
const osMap   = { darwin: 'darwin', linux: 'linux', win32: 'windows' };

const goreleaserArch = archMap[process.arch];
const goreleaserOS   = osMap[process.platform];
if (!goreleaserArch || !goreleaserOS) {
  console.error(`[omnigate] unsupported platform: ${process.platform}/${process.arch}`);
  console.error('  Open an issue: https://github.com/yunqilee69/OmniGate/issues');
  process.exit(1);
}

const archiveName   = `omnigate-${VERSION}-${goreleaserOS}-${goreleaserArch}.tar.gz`;
const checksumsName = `omnigate_${VERSION}_checksums.txt`;
const releaseBase   = `https://github.com/yunqilee69/OmniGate/releases/download/v${VERSION}`;
const r2Base        = `https://cdn.yunke.icu/v${VERSION}`;

// 双源策略:优先 Cloudflare R2(中国大陆快),失败回退 GitHub Release(全球可用)
const downloadUrl   = `${releaseBase}/${archiveName}`;
const checksumsUrl  = `${releaseBase}/${checksumsName}`;
const r2ArchiveUrl  = `${r2Base}/${archiveName}`;
const r2ChecksumUrl = `${r2Base}/${checksumsName}`;

const vendorDir     = path.join(__dirname, '..', 'vendor');
const binName       = process.platform === 'win32' ? 'omnigate.exe' : 'omnigate';
const binPath       = path.join(vendorDir, binName);

// 可选 GitHub 加速镜像(如 ghproxy.com / ghfast.top),格式 https://镜像前缀/https://原URL。
// 仅作用于 GitHub 归档包下载;R2 与 GitHub 校验和始终直连官方源(镜像投毒会被 sha256 拦截)。
const ghProxy = (process.env.OMNIGATE_GH_PROXY || '').replace(/\/+$/, '');
const ghArchiveUrlProxied = ghProxy ? `${ghProxy}/${downloadUrl}` : downloadUrl;

if (fs.existsSync(binPath)) {
  console.log(`[omnigate] binary already present at vendor/${binName}, skipping download`);
  process.exit(0);
}

console.log(`[omnigate] downloading ${goreleaserOS}/${goreleaserArch} for v${VERSION}`);
fs.mkdirSync(vendorDir, { recursive: true });

// 单次请求。空闲超时按 socket 数据流动重置,慢而不断流的下载不会被误杀;
// autoSelectFamily 开启 IPv4/IPv6 竞速建连,规避 v6 路由黑洞导致的挂起
// (curl 内置此行为,Node 需显式开启)。OMNIGATE_DOWNLOAD_TIMEOUT 单位秒,默认 30。
const STALL_TIMEOUT_MS = (() => {
  const v = parseInt(process.env.OMNIGATE_DOWNLOAD_TIMEOUT, 10);
  return Number.isFinite(v) && v > 0 ? v * 1000 : 30000;
})();

function fetchOnce(url, redirectsLeft) {
  if (redirectsLeft > 5) return Promise.reject(new Error('too many redirects'));
  return new Promise((resolve, reject) => {
    const req = https.get(url, {
      headers: { 'user-agent': 'omnigate-npm-installer' },
      autoSelectFamily: true,
    }, (res) => {
      if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
        res.resume();
        return resolve(fetchOnce(new URL(res.headers.location, url).toString(), redirectsLeft + 1));
      }
      if (res.statusCode !== 200) {
        res.resume();
        return reject(new Error(`HTTP ${res.statusCode} (${url})`));
      }
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => resolve(Buffer.concat(chunks)));
      res.on('error', reject);
    });
    req.on('error', reject);
    req.setTimeout(STALL_TIMEOUT_MS, () => req.destroy(new Error(`download stalled (no data for ${STALL_TIMEOUT_MS / 1000}s)`)));
  });
}

// 3 次重试 + 线性退避:瞬时网络抖动不应让安装直接失败。
// 双源策略:R2(中国快)→ GitHub 镜像(如配置)→ GitHub 直连,三级回退
function download(url) {
  const attempt = (n) => fetchOnce(url, 0).catch((err) => {
    if (n >= 3) throw err;
    console.warn(`[omnigate] download attempt ${n} failed (${err.message}), retrying…`);
    return new Promise((r) => setTimeout(r, 1000 * n)).then(() => attempt(n + 1));
  });
  return attempt(1);
}

async function downloadWithFallback(primaryUrl, fallbackUrl, description) {
  try {
    return await download(primaryUrl);
  } catch (primaryErr) {
    console.warn(`[omnigate] ${description} download from primary failed, trying fallback…`);
    try {
      return await download(fallbackUrl);
    } catch (fallbackErr) {
      throw new Error(`Both sources failed. Primary: ${primaryErr.message}; Fallback: ${fallbackErr.message}`);
    }
  }
}

// goreleaser checksums 行格式: "<sha256hex>  <filename>"
function expectedChecksum(checksumsText, fileName) {
  for (const line of checksumsText.split('\n')) {
    const parts = line.trim().split(/\s+/);
    if (parts.length >= 2 && parts[parts.length - 1] === fileName) {
      return parts[0].toLowerCase();
    }
  }
  return null;
}

(async () => {
  const tmpPath = path.join(vendorDir, archiveName + '.tmp');
  try {
    const [archive, checksums] = await Promise.all([
      downloadWithFallback(r2ArchiveUrl, ghArchiveUrlProxied, 'archive'),
      downloadWithFallback(r2ChecksumUrl, checksumsUrl, 'checksums'),
    ]);

    // fail closed: 校验文件缺失、条目缺失、哈希不匹配都拒绝安装
    const expected = expectedChecksum(checksums.toString('utf8'), archiveName);
    if (!expected) {
      throw new Error(`no checksum entry for ${archiveName} in ${checksumsName} — refusing to install`);
    }
    const actual = crypto.createHash('sha256').update(archive).digest('hex');
    if (actual !== expected) {
      throw new Error(`sha256 mismatch for ${archiveName}: expected ${expected}, got ${actual} — refusing to install`);
    }
    console.log(`[omnigate] sha256 verified ✓ (${expected.slice(0, 12)}…)`);

    fs.writeFileSync(tmpPath, archive);
    const filter = (p) =>
      p === 'omnigate' || p === 'omnigate.exe' ||
      p.endsWith('/omnigate') || p.endsWith('/omnigate.exe');
    await tar.x({ file: tmpPath, cwd: vendorDir, filter });

    if (!fs.existsSync(binPath)) {
      throw new Error(`extracted archive but ${binName} not found (format changed?)`);
    }
    if (process.platform !== 'win32') {
      fs.chmodSync(binPath, 0o755);
    }
    console.log('[omnigate] installed ✓  Run `omnigate --version` to verify.');
  } catch (err) {
    console.error(`[omnigate] postinstall failed: ${err.message}`);
    console.error(`  Try again, or download manually:`);
    console.error(`    ${downloadUrl}`);
    console.error(`  Issues: https://github.com/yunqilee69/OmniGate/issues`);
    process.exit(1);
  } finally {
    try { fs.unlinkSync(path.join(vendorDir, archiveName + '.tmp')); } catch (_) { /* not exist */ }
  }
})();
