import { gzipSync } from "node:zlib";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { extname, join, relative } from "node:path";

const distIdx = process.argv.indexOf("--dist");
const DIST_ASSETS_DIR =
  distIdx >= 0 && process.argv[distIdx + 1]
    ? process.argv[distIdx + 1]
    : join(process.cwd(), "dist", "assets");
const emitJSON = process.argv.includes("--json");

// Largest-chunk budgets: a single JS file cannot exceed these.
const MAX_RAW_BYTES = Number.parseInt(process.env.BUNDLE_MAX_BYTES ?? "1400000", 10);
const MAX_GZIP_BYTES = Number.parseInt(process.env.BUNDLE_MAX_GZIP_BYTES ?? "430000", 10);
// Total-route-assets budgets: the sum of every file the console actually
// downloads. Splitting one oversize chunk into N under-limit chunks cannot
// evade this the way a largest-chunk-only check can.
const TOTAL_MAX_RAW_BYTES = Number.parseInt(process.env.BUNDLE_TOTAL_MAX_BYTES ?? "5000000", 10);
const TOTAL_MAX_GZIP_BYTES = Number.parseInt(
  process.env.BUNDLE_TOTAL_MAX_GZIP_BYTES ?? "1600000",
  10,
);

const ROUTE_ASSET_EXT = new Set([
  ".js",
  ".css",
  ".wasm",
  ".woff",
  ".woff2",
  ".ttf",
  ".otf",
  ".eot",
  ".png",
  ".jpg",
  ".jpeg",
  ".gif",
  ".svg",
  ".webp",
  ".avif",
  ".ico",
]);

function listFiles(dir, acc = []) {
  let entries;
  try {
    entries = readdirSync(dir, { withFileTypes: true });
  } catch (err) {
    console.error(`Cannot read ${dir}: ${err.message}`);
    process.exit(1);
  }
  for (const entry of entries) {
    const fullPath = join(dir, entry.name);
    if (entry.isDirectory()) {
      listFiles(fullPath, acc);
      continue;
    }
    if (!entry.isFile()) continue;
    if (entry.name.endsWith(".map")) continue;
    const ext = extname(entry.name).toLowerCase();
    if (!ROUTE_ASSET_EXT.has(ext)) continue;
    acc.push(fullPath);
  }
  return acc;
}

const assetFiles = listFiles(DIST_ASSETS_DIR);
const jsFiles = assetFiles.filter((file) => file.endsWith(".js"));

if (jsFiles.length === 0) {
  console.error(`No JS assets found in ${DIST_ASSETS_DIR}. Build output format may have changed.`);
  process.exit(1);
}

const statsOf = (file) => {
  const rawBytes = statSync(file).size;
  const gzipBytes = gzipSync(readFileSync(file)).length;
  return { file: relative(DIST_ASSETS_DIR, file), rawBytes, gzipBytes };
};

const bundleStats = jsFiles.map(statsOf);
bundleStats.sort((a, b) => b.rawBytes - a.rawBytes);
const largestRaw = bundleStats[0];
const largestGzip = bundleStats.reduce((max, current) =>
  current.gzipBytes > max.gzipBytes ? current : max,
);

const routeStats = assetFiles.map(statsOf);
const totalRaw = routeStats.reduce((sum, item) => sum + item.rawBytes, 0);
const totalGzip = routeStats.reduce((sum, item) => sum + item.gzipBytes, 0);

const formatKiB = (bytes) => `${(bytes / 1024).toFixed(2)} KiB`;
const report = emitJSON ? console.error.bind(console) : console.log.bind(console);

report(`Largest raw JS chunk : ${largestRaw.file} (${formatKiB(largestRaw.rawBytes)})`);
report(`Largest gzip JS chunk: ${largestGzip.file} (${formatKiB(largestGzip.gzipBytes)})`);
report(`Budget raw  <= ${formatKiB(MAX_RAW_BYTES)}`);
report(`Budget gzip <= ${formatKiB(MAX_GZIP_BYTES)}`);
report(
  `Total route assets   : ${routeStats.length} files, raw ${formatKiB(totalRaw)}, gzip ${formatKiB(totalGzip)}`,
);
report(`Total budget raw     <= ${formatKiB(TOTAL_MAX_RAW_BYTES)}`);
report(`Total budget gzip    <= ${formatKiB(TOTAL_MAX_GZIP_BYTES)}`);

const errors = [];
if (largestRaw.rawBytes > MAX_RAW_BYTES) {
  errors.push(
    `Raw chunk budget exceeded: ${largestRaw.file} is ${largestRaw.rawBytes} bytes (limit ${MAX_RAW_BYTES}).`,
  );
}
if (largestGzip.gzipBytes > MAX_GZIP_BYTES) {
  errors.push(
    `Gzip chunk budget exceeded: ${largestGzip.file} is ${largestGzip.gzipBytes} bytes (limit ${MAX_GZIP_BYTES}).`,
  );
}
if (totalRaw > TOTAL_MAX_RAW_BYTES) {
  errors.push(
    `Total route-asset raw budget exceeded: ${totalRaw} bytes across ${routeStats.length} files (limit ${TOTAL_MAX_RAW_BYTES}). Splitting a large chunk cannot evade this.`,
  );
}
if (totalGzip > TOTAL_MAX_GZIP_BYTES) {
  errors.push(
    `Total route-asset gzip budget exceeded: ${totalGzip} bytes across ${routeStats.length} files (limit ${TOTAL_MAX_GZIP_BYTES}). Splitting a large chunk cannot evade this.`,
  );
}

if (emitJSON) {
  process.stdout.write(
    `${JSON.stringify({
      largest_js_raw_bytes: largestRaw.rawBytes,
      largest_js_gzip_bytes: largestGzip.gzipBytes,
      largest_js_raw_file: largestRaw.file,
      largest_js_gzip_file: largestGzip.file,
      total_raw_bytes: totalRaw,
      total_gzip_bytes: totalGzip,
      file_count: routeStats.length,
      budgets: {
        largest_js_raw_bytes: MAX_RAW_BYTES,
        largest_js_gzip_bytes: MAX_GZIP_BYTES,
        total_raw_bytes: TOTAL_MAX_RAW_BYTES,
        total_gzip_bytes: TOTAL_MAX_GZIP_BYTES,
      },
      ok: errors.length === 0,
      errors,
    })}\n`,
  );
}

if (errors.length > 0) {
  for (const error of errors) {
    console.error(error);
  }
  process.exit(1);
}
