#!/usr/bin/env node
// postinstall: 按当前 OS/arch 从 GitHub Releases 拉对应 tar.gz,
// 解压到 vendor/,chmod +x(类 unix)。包版本号必须与 GitHub Release tag 一致。

const https = require('https');
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

const archiveName = `omnigate-${VERSION}-${goreleaserOS}-${goreleaserArch}.tar.gz`;
const downloadUrl = `https://github.com/cloudomni/omnigate/releases/download/v${VERSION}/${archiveName}`;
const vendorDir   = path.join(__dirname, '..', 'vendor');
const binName     = process.platform === 'win32' ? 'omnigate.exe' : 'omnigate';
const binPath     = path.join(vendorDir, binName);

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
      resolve(res);
    });
    req.on('error', reject);
    req.setTimeout(60000, () => req.destroy(new Error('download timeout (60s)')));
  });
}

(async () => {
  try {
    const res = await download(downloadUrl);
    const filter = (p) =>
      p === 'omnigate' || p === 'omnigate.exe' ||
      p.endsWith('/omnigate') || p.endsWith('/omnigate.exe');
    await tar.x({ cwd: vendorDir, filter });

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
  }
})();
