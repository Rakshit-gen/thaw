// Copies the benchmark results the Go side wrote into the site, so a build
// always shows the numbers that are in the repo. A missing file becomes
// null and the pages say that benchmark has not been run.
import { copyFileSync, existsSync, mkdirSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const results = join(here, "..", "..", "results");
const data = join(here, "..", "src", "data");
mkdirSync(data, { recursive: true });
for (const name of ["vm.json", "sim.json"]) {
  const from = join(results, name);
  if (existsSync(from)) copyFileSync(from, join(data, name));
  else writeFileSync(join(data, name), "null\n");
  console.log(`${name}: ${existsSync(from) ? "copied" : "missing, wrote null"}`);
}
