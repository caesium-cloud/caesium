import { gzipSync } from "node:zlib";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";

const DIST_ASSETS_DIR = join(process.cwd(), "dist", "assets");
const MAX_RAW_BYTES = Number.parseInt(process.env.BUNDLE_MAX_BYTES ?? "1400000", 10);
const MAX_GZIP_BYTES = Number.parseInt(process.env.BUNDLE_MAX_GZIP_BYTES ?? "430000", 10);

const jsFiles = readdirSync(DIST_ASSETS_DIR).filter((file) => file.endsWith(".js"));

if (jsFiles.length === 0) {
  console.error("No JS assets found in dist/assets. Build output format may have changed.");
  process.exit(1);
}

const bundleStats = jsFiles.map((file) => {
  const fullPath = join(DIST_ASSETS_DIR, file);
  const rawBytes = statSync(fullPath).size;
  const gzipBytes = gzipSync(readFileSync(fullPath)).length;
  return { file, rawBytes, gzipBytes };
});

bundleStats.sort((a, b) => b.rawBytes - a.rawBytes);
const largestRaw = bundleStats[0];
const largestGzip = bundleStats.reduce((max, current) =>
  current.gzipBytes > max.gzipBytes ? current : max,
);

const formatKiB = (bytes) => `${(bytes / 1024).toFixed(2)} KiB`;

console.log(`Largest raw JS chunk : ${largestRaw.file} (${formatKiB(largestRaw.rawBytes)})`);
console.log(`Largest gzip JS chunk: ${largestGzip.file} (${formatKiB(largestGzip.gzipBytes)})`);
console.log(`Budget raw  <= ${formatKiB(MAX_RAW_BYTES)}`);
console.log(`Budget gzip <= ${formatKiB(MAX_GZIP_BYTES)}`);

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

if (errors.length > 0) {
  for (const error of errors) {
    console.error(error);
  }
  process.exit(1);
}
