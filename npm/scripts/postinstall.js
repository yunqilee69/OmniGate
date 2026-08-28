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
  console.error('  Open an issue: https://github.com/cloudomni/omnigate/issues');
  process.exit(1);
}

const archiveName   = `omnigate-${VERSION}-${goreleaserOS}-${goreleaserArch}.tar.gz`;
const checksumsName = `omnigate_${VERSION}_checksums.txt`;
const releaseBase   = `https://github.com/cloudomni/omnigate/releases/download/v${VERSION}`;
const downloadUrl   = `${releaseBase}/${archiveName}`;
const checksumsUrl  = `${releaseBase}/${checksumsName}`;
const vendorDir     = path.join(__dirname, '..', 'vendor');
const binName       = process.platform === 'win32' ? 'omnigate.exe' : 'omnigate';
const binPath       = path.join(vendorDir, binName);

if (fs.existsSync(binPath)) {
  console.log(`[omnigate] binary already present at vendor/${binName}, skipping download`);
  process.exit(0);
}

console.log(`[omnigate] downloading ${goreleaserOS}/${goreleaserArch} for v${VERSION}`);
fs.mkdirSync(vendorDir, { recursive: true });

function download(url, redirectsLeft) {
  redirectsLeft = redirectsLeft || 0;
  if (redirectsLeft > 5) return Promise.reject(new Error('too many redirects'));
  return new Promise((resolve, reject) => {
    const req = https.get(url, { headers: { 'user-agent': 'omnigate-npm-installer' } }, (res) => {
      if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
        res.resume();
        const next = new URL(res.headers.location, url).toString();
        return resolve(download(next, redirectsLeft + 1));
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
    req.setTimeout(60000, () => req.destroy(new Error('download timeout (60s)')));
  });
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
      download(downloadUrl),
      download(checksumsUrl),
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
    console.error(`  Issues: https://github.com/cloudomni/omnigate/issues`);
    process.exit(1);
  } finally {
    try { fs.unlinkSync(path.join(vendorDir, archiveName + '.tmp')); } catch (_) { /* not exist */ }
  }
})();
