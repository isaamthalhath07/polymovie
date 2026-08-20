/**
 * Amazon Prime Video — TTML2 Subtitle Grabber  (speed-optimised)
 * ──────────────────────────────────────────────────────────────
 * REQUIREMENTS
 *   node >= 18
 *   npm install puppeteer
 *
 * FIRST RUN  →  logs you in and saves the session to persistDir
 * LATER RUNS →  goes straight to the show (no login step)
 *
 *   node prime_subtitle_grabber.js
 */

'use strict';

const puppeteer = require('puppeteer');
const fs        = require('fs');
const path      = require('path');

// ═══════════════════════════════════════════════════════════════════════════
//  ▶  CONFIGURE THIS BLOCK
// ═══════════════════════════════════════════════════════════════════════════

const CONFIG = {
  showUrl:    'https://www.amazon.com/gp/video/detail/REPLACE_WITH_ASIN',
  season:     1,
  episode:    3,
  outputDir:  '.',

  // Persists login cookies — you only log in once, ever.  Folder is auto-created.
  persistDir: './chrome-prime-profile',

  // Safety ceiling: script resolves immediately on first capture, so this is
  // only hit if something goes wrong.
  subtitleTimeoutMs: 30_000,
};

// ═══════════════════════════════════════════════════════════════════════════

const SUBTITLE_HOST = 'cf-timedtext.aux.pv-cdn.net';

// Tracking/ad domains blocked on every page load to cut load time
const BLOCKED_URL_PATTERNS = [
  '*doubleclick.net*', '*googlesyndication.com*', '*googletagmanager.com*',
  '*amazon-adsystem.com*', '*assoc-amazon.com*', '*scorecardresearch.com*',
  '*omtrdc.net*', '*demdex.net*', '*adsafeprotected.com*', '*moatads.com*',
];

// ── tiny helpers ─────────────────────────────────────────────────────────────

const t0      = Date.now();
const elapsed = () => Date.now() - t0;
const log     = (...a) => console.log(`[${elapsed()}ms]`, ...a);

function saveFile(content, url) {
  const base    = path.basename(new URL(url).pathname).split('?')[0] || 'subtitles';
  const name    = /\.ttml/.test(base) ? base : base + '.ttml2';
  const outPath = path.join(CONFIG.outputDir, `${Date.now()}_${name}`);
  fs.writeFileSync(outPath, content, 'utf8');
  return outPath;
}

// ── main ─────────────────────────────────────────────────────────────────────

(async () => {

  // ── 1. Launch — persistent profile + speed flags ───────────────────────────
  log('Launching browser…');
  const browser = await puppeteer.launch({
    headless:        false,
    userDataDir:     CONFIG.persistDir,   // reuse cookies between runs
    defaultViewport: null,
    args: [
      '--start-maximized',
      '--no-sandbox',
      '--disable-setuid-sandbox',
      '--disable-blink-features=AutomationControlled',
      '--disable-background-networking',
      '--disable-background-timer-throttling',
      '--disable-backgrounding-occluded-windows',
      '--disable-client-side-phishing-detection',
      '--disable-default-apps',
      '--disable-extensions',
      '--disable-hang-monitor',
      '--disable-sync',
      '--disable-translate',
      '--metrics-recording-only',
      '--no-first-run',
      '--safebrowsing-disable-auto-update',
    ],
    ignoreDefaultArgs: ['--enable-automation'],
  });

  const page = await browser.newPage();

  // Hide automation fingerprint
  await page.evaluateOnNewDocument(() => {
    Object.defineProperty(navigator, 'webdriver', { get: () => undefined });
  });

  // ── 2. CDP: wire up interception BEFORE any navigation ─────────────────────
  // Setting this up first means we cannot miss a response regardless of how
  // quickly the player requests the subtitle file.
  const client = await page.target().createCDPSession();
  await client.send('Network.enable');
  await client.send('Network.setBlockedURLs', { urls: BLOCKED_URL_PATTERNS });

  // Promise that resolves the instant we capture the first subtitle file
  let captureResolve;
  const capturePromise = new Promise(r => { captureResolve = r; });
  const captured       = [];

  function attachSubtitleListener(cdpClient) {
    cdpClient.on('Network.responseReceived', async (ev) => {
      const url = ev.response.url;
      if (!url.includes(SUBTITLE_HOST) || !/ttml/i.test(url)) return;

      log(`🎯 Subtitle hit: ${url}`);
      try {
        const { body, base64Encoded } = await cdpClient.send(
          'Network.getResponseBody', { requestId: ev.requestId },
        );
        const content = base64Encoded
          ? Buffer.from(body, 'base64').toString('utf8')
          : body;
        const outPath = saveFile(content, url);
        captured.push(outPath);
        log(`💾 Saved → ${outPath}  (${content.length.toLocaleString()} chars)`);
        captureResolve(outPath);         // ← immediately resolve; don't wait out timeout
      } catch (err) {
        log(`⚠️  Body fetch failed: ${err.message}`);
      }
    });
  }

  attachSubtitleListener(client);

  // Also watch for new tabs (Amazon sometimes opens the player in a new page)
  browser.on('targetcreated', async (target) => {
    if (target.type() !== 'page') return;
    try {
      const newPage   = await target.page();
      const newClient = await newPage.target().createCDPSession();
      await newClient.send('Network.enable');
      attachSubtitleListener(newClient);
      log('📄 Attached subtitle listener to new tab.');
    } catch (_) {}
  });

  // ── 3. Navigate directly to the show ──────────────────────────────────────
  // We skip the sign-in page entirely; Amazon auto-redirects if not logged in.
  // Using 'domcontentloaded' instead of 'networkidle2' shaves 1-3 seconds.
  log(`Navigating to show…`);
  await page.goto(CONFIG.showUrl, { waitUntil: 'domcontentloaded' });

  // ── 4. Handle login if redirected ─────────────────────────────────────────
  if (/\/ap\/(signin|mfa)/.test(page.url())) {
    log('⏳ Not logged in — please sign in. Script resumes automatically.');
    await page.waitForFunction(
      () => !/\/ap\/(signin|mfa)/.test(location.href),
      { timeout: 5 * 60_000 },
    );
    log(`✅ Logged in (${elapsed()}ms)`);
    if (!page.url().includes('video/detail')) {
      await page.goto(CONFIG.showUrl, { waitUntil: 'domcontentloaded' });
    }
  }

  // ── 5. Wait for the episode list (selector-driven, not time-based) ─────────
  log('Waiting for episode list…');
  await page.waitForSelector(
    '[data-testid*="episode"], .dv-episode-item, .av-episode-item, .dv-node-dp-episodes',
    { timeout: 12_000 },
  ).catch(() => log('⚠️  Episode list not found — continuing.'));

  // ── 6. Season selection — one round-trip to the browser ────────────────────
  log(`Selecting season ${CONFIG.season}…`);
  const seasonOk = await page.evaluate((target) => {
    // <select> dropdown
    const sel = document.querySelector(
      'select[id*="season"], select[data-testid*="season"], .dv-node-dp-seasons select',
    );
    if (sel) {
      sel.value = String(target);
      sel.dispatchEvent(new Event('change', { bubbles: true }));
      return true;
    }
    // Tab / button style
    const tab = [...document.querySelectorAll(
      '[data-testid*="season"], .dv-season-selector li, .av-season-selector li, li[class*="season"], [class*="season-selector"] li',
    )].find(el => el.textContent.trim().includes(String(target)));
    if (tab) { tab.click(); return true; }
    return false;
  }, CONFIG.season);

  if (!seasonOk) {
    log('⚠️  Could not auto-select season. Please select it manually, then press ENTER.');
    await waitForEnter();
  }

  // Wait for the episode list to refresh (selector-driven)
  await page.waitForSelector(
    '[data-testid*="episode"], .dv-episode-item, .av-episode-item',
    { timeout: 8_000 },
  ).catch(() => {});

  // ── 7. Episode selection — one round-trip to the browser ───────────────────
  log(`Clicking episode ${CONFIG.episode}…`);
  const episodeOk = await page.evaluate((target) => {
    // Structured episode cards (index-based)
    const cards = [...document.querySelectorAll(
      '[data-testid*="episode"], .dv-episode-item, .av-episode-item, article[class*="episode"]',
    )];
    const card = cards[target - 1];
    if (card) {
      const btn = card.querySelector(
        'button[aria-label*="Play"], button[data-testid*="play"], a[href*="video"]',
      );
      (btn || card).click();
      return true;
    }
    // Fallback: match by leading episode number in text
    const hit = [...document.querySelectorAll('li, article, div[class*="episode"]')]
      .find(el => { const m = el.textContent.trim().match(/^(\d+)[.\s]/); return m && +m[1] === target; });
    if (hit) {
      const btn = hit.querySelector('button, a[href*="video"]');
      (btn || hit).click();
      return true;
    }
    return false;
  }, CONFIG.episode);

  if (!episodeOk) {
    log('⚠️  Could not auto-click episode. Start playback manually, then press ENTER.');
    await waitForEnter();
  }

  // ── 8. Wait for the video player (handles same-page and new-tab cases) ─────
  log('Waiting for video player…');
  const playerPage = await waitForPlayer(browser, page);
  log(`✅ Player ready (${elapsed()}ms)`);

  // ── 9. Auto-enable captions (subtitles only download when CC is on) ─────────
  log('Enabling captions…');
  await autoEnableCaptions(playerPage);

  // ── 10. Await subtitle capture (resolves immediately on first hit) ──────────
  log(`⏳ Waiting for subtitle file (ceiling: ${CONFIG.subtitleTimeoutMs / 1000}s)…`);
  const result = await Promise.race([
    capturePromise,
    new Promise((_, rej) =>
      setTimeout(() => rej(new Error('timeout')), CONFIG.subtitleTimeoutMs)
    ),
  ]).catch(err => err);

  if (result instanceof Error) {
    log('❌ No subtitle file captured.');
    log('   • Confirm captions are ON in the player.');
    log('   • Check DevTools → Network → filter "ttml" to verify the request URL.');
    log(`   • Try a larger subtitleTimeoutMs (currently ${CONFIG.subtitleTimeoutMs}).`);
  } else {
    log(`\n✅ Finished in ${elapsed()}ms — ${captured.length} file(s):`);
    captured.forEach(f => log('  ', f));
  }

  await browser.close();
})();

// ── waitForPlayer ─────────────────────────────────────────────────────────────
// Returns whichever page the player appears on first (same tab or new tab).

async function waitForPlayer(browser, originalPage) {
  const SEL     = 'video, .atvwebplayersdk-player-container, [data-testid="video-player"]';
  const TIMEOUT = 15_000;

  return Promise.race([
    originalPage.waitForSelector(SEL, { timeout: TIMEOUT }).then(() => originalPage),
    new Promise((resolve) => {
      browser.on('targetcreated', async (target) => {
        if (target.type() !== 'page') return;
        const p = await target.page().catch(() => null);
        if (!p) return;
        p.waitForSelector(SEL, { timeout: TIMEOUT }).then(() => resolve(p)).catch(() => {});
      });
    }),
  ]).catch(() => originalPage);
}

// ── autoEnableCaptions ────────────────────────────────────────────────────────
// Clicks the CC button and selects the first language option in one shot.

async function autoEnableCaptions(page) {
  const CC_SELECTORS = [
    '.atvwebplayersdk-subtitle-button',
    '[data-testid="subtitles-button"]',
    'button[aria-label*="ubtitle"]',
    'button[aria-label*="aption"]',
    'button[title*="ubtitle"]',
    'button[title*="aption"]',
  ];
  const MENU_SELECTORS = [
    '.atvwebplayersdk-subtitles-menu [role="menuitem"]',
    '[data-testid="subtitle-menu-item"]',
    '[role="menuitem"]',
  ];

  // Wait for controls to render
  await page.waitForSelector(CC_SELECTORS.join(', '), { timeout: 10_000, visible: true })
    .catch(() => {});

  // Single evaluate: click CC button, then pick first menu item — two clicks, one trip
  await page.evaluate((ccSels, menuSels) => {
    for (const s of ccSels) {
      const btn = document.querySelector(s);
      if (btn) { btn.click(); break; }
    }
    // Menu may not exist yet; a short rAF wait is enough in-browser
    requestAnimationFrame(() => {
      for (const s of menuSels) {
        const item = document.querySelector(s);
        if (item) { item.click(); break; }
      }
    });
  }, CC_SELECTORS, MENU_SELECTORS).catch(() => {});
}

// ── waitForEnter ──────────────────────────────────────────────────────────────

function waitForEnter() {
  log('  → Press ENTER to continue…');
  return new Promise(resolve => {
    process.stdin.setRawMode?.(false);
    process.stdin.resume();
    process.stdin.once('data', () => { process.stdin.pause(); resolve(); });
  });
}
