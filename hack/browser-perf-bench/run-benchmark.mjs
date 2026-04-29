#!/usr/bin/env node
/**
 * Automated browser performance benchmark for Argo CD /api/v1/applications endpoint.
 *
 * Uses Puppeteer to drive Chrome with DevTools Protocol for precise heap and CPU measurements.
 * Requires applications to exist in the cluster — create them first with:
 *   ./hack/generate-scale-test-apps.sh 1000
 *
 * Usage:
 *   node run-benchmark.mjs --url=https://localhost:8080 --token=<argocd-token>
 *   node run-benchmark.mjs --url=https://localhost:8080 --token=<token> --iterations=5
 *   node run-benchmark.mjs --url=https://localhost:8080 --token=<token> --trace --no-headless
 *
 * Flags:
 *   --url=URL               Argo CD API URL (required)
 *   --token=TOKEN           Argo CD bearer token (required)
 *   --iterations=N          Iterations for averaging (default: 3)
 *   --output=DIR            Output directory (default: benchmark-results/<timestamp>)
 *   --headless              Run in headless mode (default: true)
 *   --no-headless           Run with visible browser (useful for debugging)
 *   --trace                 Capture Chrome trace for each run (saved to output dir)
 */

import { existsSync, mkdirSync, writeFileSync, readFileSync } from 'fs';
import { join, dirname } from 'path';
import { fileURLToPath } from 'url';
import { request as httpsRequest } from 'https';
import { request as httpRequest } from 'http';

const __dirname = dirname(fileURLToPath(import.meta.url));

function parseArgs() {
  const args = process.argv.slice(2);
  const opts = {
    iterations: 3,
    output: null,
    url: '',
    token: '',
    headless: true,
    trace: false,
  };

  for (const arg of args) {
    if (arg.startsWith('--iterations=')) opts.iterations = parseInt(arg.slice(13));
    else if (arg.startsWith('--output=')) opts.output = arg.slice(9);
    else if (arg.startsWith('--url=')) opts.url = arg.slice(6);
    else if (arg.startsWith('--token=')) opts.token = arg.slice(8);
    else if (arg === '--headless') opts.headless = true;
    else if (arg === '--no-headless') opts.headless = false;
    else if (arg === '--trace') opts.trace = true;
    else if (arg === '--help' || arg === '-h') {
      console.log(readFileSync(join(__dirname, 'run-benchmark.mjs'), 'utf8').match(/\/\*\*([\s\S]*?)\*\//)?.[1] || '');
      process.exit(0);
    }
  }

  if (!opts.url) {
    console.error('Error: --url is required. Example: --url=https://localhost:8080');
    process.exit(1);
  }
  if (!opts.token) {
    console.error('Error: --token is required. Example: --token=$(argocd account generate-token)');
    process.exit(1);
  }

  if (!opts.output) {
    const ts = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
    opts.output = join(__dirname, '..', '..', 'benchmark-results', ts);
  }

  return opts;
}

const APP_LIST_FIELDS = [
  'metadata.resourceVersion',
  'items.metadata.name', 'items.metadata.namespace', 'items.metadata.annotations',
  'items.metadata.labels', 'items.metadata.creationTimestamp', 'items.metadata.deletionTimestamp',
  'items.spec', 'items.operation.sync',
  'items.status.sync.status', 'items.status.sync.revision', 'items.status.health',
  'items.status.operationState.phase', 'items.status.operationState.finishedAt',
  'items.status.operationState.operation.sync', 'items.status.summary', 'items.status.resources'
].join(',');

function fetchFromAPI(apiUrl, token) {
  return new Promise((resolve, reject) => {
    const url = new URL(`/api/v1/applications?fields=${APP_LIST_FIELDS}`, apiUrl);
    const isHttps = url.protocol === 'https:';
    const requestFn = isHttps ? httpsRequest : httpRequest;

    const opts = {
      hostname: url.hostname,
      port: url.port || (isHttps ? 443 : 80),
      path: url.pathname + url.search,
      method: 'GET',
      headers: {
        'Authorization': `Bearer ${token}`,
        'Accept': 'application/json',
      },
      rejectUnauthorized: false,
    };

    const req = requestFn(opts, res => {
      if (res.statusCode !== 200) {
        reject(new Error(`API returned ${res.statusCode}: ${res.statusMessage}`));
        res.resume();
        return;
      }
      const chunks = [];
      res.on('data', chunk => chunks.push(chunk));
      res.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    });

    req.on('error', reject);
    req.end();
  });
}

async function main() {
  const opts = parseArgs();
  mkdirSync(opts.output, { recursive: true });

  let puppeteer;
  try {
    puppeteer = await import('puppeteer');
  } catch {
    console.error('Puppeteer not found. Install it:\n  npm install puppeteer\n  npx puppeteer browsers install chrome');
    process.exit(1);
  }

  // Fetch the JSON from the API
  console.log(`Fetching applications from ${opts.url} ...`);
  let liveJsonString;
  try {
    liveJsonString = await fetchFromAPI(opts.url, opts.token);
    const parsed = JSON.parse(liveJsonString);
    const appCount = parsed.items?.length || 0;
    console.log(`Fetched ${appCount} applications (${(liveJsonString.length / 1048576).toFixed(1)} MB)`);
    writeFileSync(join(opts.output, 'api-response.json'), liveJsonString);
  } catch (err) {
    console.error(`\nFailed to fetch from API: ${err.message}`);
    console.error(`Check your --url and --token flags.`);
    process.exit(1);
  }

  console.log('');
  console.log('=== Argo CD UI Performance Benchmark ===');
  console.log(`Iterations: ${opts.iterations}`);
  console.log(`Output: ${opts.output}`);
  console.log('');

  const browser = await puppeteer.default.launch({
    headless: opts.headless ? 'new' : false,
    args: [
      '--enable-precise-memory-info',
      '--js-flags=--expose-gc',
      '--disable-extensions',
      '--disable-gpu',
      '--no-sandbox',
      '--max-old-space-size=8192',
      '--ignore-certificate-errors',
      '--disable-application-cache',
      '--disk-cache-size=0',
    ],
    ignoreHTTPSErrors: true,
  });

  const page = await browser.newPage();
  const client = await page.createCDPSession();

  // Disable cache so fresh JS/CSS is always loaded
  await page.setCacheEnabled(false);

  const benchmarkPath = join(__dirname, 'index.html');
  await page.goto(`file://${benchmarkPath}`, { waitUntil: 'networkidle0' });

  const iterResults = [];

  for (let iter = 0; iter < opts.iterations; iter++) {
    const pct = (((iter + 1) / opts.iterations) * 100).toFixed(0);
    process.stdout.write(`\r[${pct}%] Iteration ${iter + 1}/${opts.iterations}...`);

    // Clear all caches, cookies, and storage before each iteration
    await client.send('Network.clearBrowserCache');
    await client.send('Network.clearBrowserCookies');
    await client.send('Storage.clearDataForOrigin', {
      origin: `file://`,
      storageTypes: 'all',
    }).catch(() => {});

    // Reload the page to pick up any script changes
    await page.reload({ waitUntil: 'networkidle0' });

    // Force GC before measurement
    await client.send('HeapProfiler.collectGarbage');
    await new Promise(r => setTimeout(r, 200));

    const heapBefore = await client.send('Runtime.getHeapUsage');

    if (opts.trace && iter === 0) {
      await client.send('Profiler.enable');
      await client.send('Profiler.start');
    }

    // Inject the JSON string and measure in-page
    await page.evaluate((json) => { window.__liveJson = json; }, liveJsonString);
    const result = await page.evaluate(async () => {
      const json = window.__liveJson;
      delete window.__liveJson;
      return await measureLiveFromString(json);
    });

    const heapAfter = await client.send('Runtime.getHeapUsage');

    if (opts.trace && iter === 0) {
      const profile = await client.send('Profiler.stop');
      const profilePath = join(opts.output, `cpu-profile.cpuprofile`);
      writeFileSync(profilePath, JSON.stringify(profile.profile));
      await client.send('Profiler.disable');
    }

    result.cdpHeapBefore = heapBefore.usedSize;
    result.cdpHeapAfter = heapAfter.usedSize;
    result.cdpHeapDelta = heapAfter.usedSize - heapBefore.usedSize;
    result.cdpTotalHeap = heapAfter.totalSize;

    iterResults.push(result);
  }

  console.log('\n');
  await browser.close();

  // Compute average
  const avg = { ...iterResults[0] };
  const numericKeys = [
    'parseTime', 'stringifyTime', 'filterTime', 'sortTime', 'normalizeTime',
    'renderPrepTime', 'filterSortTotal', 'totalCPU',
    'cdpHeapBefore', 'cdpHeapAfter', 'cdpHeapDelta', 'cdpTotalHeap'
  ];
  for (const key of numericKeys) {
    avg[key] = iterResults.reduce((sum, r) => sum + (r[key] || 0), 0) / opts.iterations;
  }
  if (avg.heapUsed) avg.heapUsed = iterResults.reduce((sum, r) => sum + (r.heapUsed || 0), 0) / opts.iterations;
  if (avg.heapDelta !== null) avg.heapDelta = iterResults.reduce((sum, r) => sum + (r.heapDelta || 0), 0) / opts.iterations;
  if (avg.parseTime > 3000) avg.severity = 'danger';
  else if (avg.parseTime > 1000) avg.severity = 'warn';
  else avg.severity = 'ok';
  avg.mainThreadBlocked = avg.parseTime > 50 ? `${(avg.parseTime / 1000).toFixed(1)}s` : 'No';

  // --- Output results ---

  // 1. JSON (all iterations + average)
  const output = { iterations: iterResults, average: avg };
  writeFileSync(join(opts.output, 'results.json'), JSON.stringify(output, null, 2));

  // 2. CSV
  const csvHeaders = ['Iteration', 'Apps', 'JSON Size (MB)', 'JSON.parse (ms)', 'Filter+Sort (ms)', 'Total CPU (ms)',
    'Heap Used (MB)', 'Heap Delta (MB)', 'CDP Heap Delta (MB)', 'Main Thread Blocked'];
  const csvRows = iterResults.map((r, i) => [
    i + 1, r.count, r.jsonSizeMB, r.parseTime.toFixed(1), r.filterSortTotal.toFixed(1), r.totalCPU.toFixed(1),
    r.heapUsed ? (r.heapUsed / 1048576).toFixed(0) : 'N/A',
    r.heapDelta ? (r.heapDelta / 1048576).toFixed(0) : 'N/A',
    (r.cdpHeapDelta / 1048576).toFixed(0),
    r.mainThreadBlocked
  ]);
  csvRows.push([
    'avg', avg.count, avg.jsonSizeMB, avg.parseTime.toFixed(1), avg.filterSortTotal.toFixed(1), avg.totalCPU.toFixed(1),
    avg.heapUsed ? (avg.heapUsed / 1048576).toFixed(0) : 'N/A',
    avg.heapDelta ? (avg.heapDelta / 1048576).toFixed(0) : 'N/A',
    (avg.cdpHeapDelta / 1048576).toFixed(0),
    avg.mainThreadBlocked
  ]);
  const csv = [csvHeaders.join(','), ...csvRows.map(r => r.join(','))].join('\n');
  writeFileSync(join(opts.output, 'results.csv'), csv);

  // 3. Markdown report
  let md = `# Argo CD UI Performance Benchmark Report\n\n`;
  md += `**Date:** ${new Date().toISOString()}\n`;
  md += `**Applications:** ${avg.count.toLocaleString()}\n`;
  md += `**Iterations:** ${opts.iterations}\n`;
  md += `**API URL:** ${opts.url}\n\n`;
  md += `## Results\n\n`;
  md += `| Iteration | Apps | JSON Size | JSON.parse | Filter+Sort | Total CPU | Heap Delta | Blocked |\n`;
  md += `|----------:|-----:|----------:|-----------:|------------:|----------:|-----------:|--------:|\n`;
  for (let i = 0; i < iterResults.length; i++) {
    const r = iterResults[i];
    md += `| ${i + 1} | ${r.count.toLocaleString()} | ${r.jsonSizeMB} MB | ${r.parseTime.toFixed(0)} ms | ${r.filterSortTotal.toFixed(0)} ms | ${r.totalCPU.toFixed(0)} ms | ${(r.cdpHeapDelta / 1048576).toFixed(0)} MB | ${r.mainThreadBlocked} |\n`;
  }
  md += `| **Avg** | ${avg.count.toLocaleString()} | ${avg.jsonSizeMB} MB | ${avg.parseTime.toFixed(0)} ms | ${avg.filterSortTotal.toFixed(0)} ms | ${avg.totalCPU.toFixed(0)} ms | ${(avg.cdpHeapDelta / 1048576).toFixed(0)} MB | ${avg.mainThreadBlocked} |\n`;

  md += `\n## Key Findings\n\n`;
  md += `1. At **${avg.count.toLocaleString()} applications**, \`JSON.parse()\` alone blocks the main thread for **${(avg.parseTime / 1000).toFixed(1)} seconds** (avg).\n`;
  md += `2. The JSON payload is **${avg.jsonSizeMB} MB** — the browser must parse this entirely before any UI can render.\n`;
  md += `3. Total CPU time on the main thread: **${(avg.totalCPU / 1000).toFixed(1)} seconds** (parse + filter + sort).\n`;
  md += `4. Heap memory increases by **${(avg.cdpHeapDelta / 1048576).toFixed(0)} MB** just from parsing the JSON response.\n`;
  md += `5. Any operation above **50ms** causes visible jank; above **100ms** feels broken to users.\n`;

  md += `\n## CPU Time Breakdown (avg over ${opts.iterations} iterations)\n\n`;
  md += `| Operation | Time (ms) | % of Total |\n`;
  md += `|-----------|----------:|-----------:|\n`;
  const ops = [
    ['JSON.parse()', avg.parseTime],
    ['Field normalization', avg.normalizeTime],
    ['Client-side filtering', avg.filterTime],
    ['Client-side sorting', avg.sortTime]
  ];
  for (const [name, time] of ops) {
    md += `| ${name} | ${time.toFixed(0)} | ${((time / avg.totalCPU) * 100).toFixed(1)}% |\n`;
  }
  md += `\n> **Conclusion:** \`JSON.parse()\` dominates at **${((avg.parseTime / avg.totalCPU) * 100).toFixed(0)}%** of total CPU time. `;
  md += `The single largest improvement would be reducing the JSON payload size sent to the browser, either via server-side pagination or by excluding heavy fields like \`status.resources\` from the list response.\n`;

  md += `\n## Reproduction\n\n`;
  md += `\`\`\`bash\n`;
  md += `# Create applications\n`;
  md += `./hack/generate-scale-test-apps.sh ${avg.count} argocd\n\n`;
  md += `# Run benchmark\n`;
  md += `./hack/browser-perf-bench/run.sh --count=${avg.count} --url=${opts.url} --token=<token> --iterations=${opts.iterations}\n`;
  md += `\`\`\`\n`;

  writeFileSync(join(opts.output, 'REPORT.md'), md);

  // 4. Console summary
  console.log('=== RESULTS ===\n');
  console.log(formatTable(iterResults, avg));
  console.log('');
  console.log(`Applications: ${avg.count.toLocaleString()}`);
  console.log(`Avg JSON.parse: ${avg.parseTime.toFixed(0)}ms (${(avg.parseTime / 1000).toFixed(1)}s main thread block)`);
  console.log(`Payload size: ${avg.jsonSizeMB} MB`);
  console.log(`Avg heap delta: ${(avg.cdpHeapDelta / 1048576).toFixed(0)} MB`);
  console.log('');
  console.log(`Results saved to: ${opts.output}/`);
  console.log(`  results.json    — raw data`);
  console.log(`  results.csv     — spreadsheet-ready`);
  console.log(`  REPORT.md       — GitHub-ready markdown report`);
  if (opts.trace) console.log(`  cpu-profile.cpuprofile — Chrome DevTools CPU profile`);
}

function formatTable(results, avg) {
  const header = 'Iteration  | Apps       | JSON Size | Parse (ms) | Filter+Sort | Total CPU | Heap Delta | Blocked';
  const sep =    '-----------|-----------|-----------|------------|-------------|-----------|------------|--------';
  const rows = results.map((r, i) =>
    `${String(i + 1).padStart(10)} | ${String(r.count).padStart(9)} | ${String(r.jsonSizeMB + ' MB').padStart(9)} | ${String(r.parseTime.toFixed(0)).padStart(10)} | ${String(r.filterSortTotal.toFixed(0) + ' ms').padStart(11)} | ${String(r.totalCPU.toFixed(0) + ' ms').padStart(9)} | ${String((r.cdpHeapDelta / 1048576).toFixed(0) + ' MB').padStart(10)} | ${r.mainThreadBlocked}`
  );
  const avgRow = `${'avg'.padStart(10)} | ${String(avg.count).padStart(9)} | ${String(avg.jsonSizeMB + ' MB').padStart(9)} | ${String(avg.parseTime.toFixed(0)).padStart(10)} | ${String(avg.filterSortTotal.toFixed(0) + ' ms').padStart(11)} | ${String(avg.totalCPU.toFixed(0) + ' ms').padStart(9)} | ${String((avg.cdpHeapDelta / 1048576).toFixed(0) + ' MB').padStart(10)} | ${avg.mainThreadBlocked}`;
  return [header, sep, ...rows, sep, avgRow].join('\n');
}

main().catch(err => {
  console.error(err);
  process.exit(1);
});
