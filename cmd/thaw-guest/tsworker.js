// A TypeScript language service over the TypeScript compiler's own sources,
// kept warm in one Node process. Building the program and checking it takes
// seconds and a few hundred megabytes; a snapshot keeps all of it, so a
// restored VM answers editor-style questions straight away.
"use strict";
const ts = require("typescript");
const fs = require("fs");
const path = require("path");
const readline = require("readline");

const root = "/work/typescript/src/compiler";
const files = fs.readdirSync(root, { recursive: true })
  .filter((f) => f.endsWith(".ts"))
  .map((f) => path.join(root, f));
const versions = new Map(files.map((f) => [f, 0]));
const options = {
  target: ts.ScriptTarget.ES2020,
  module: ts.ModuleKind.CommonJS,
  strict: true,
  noEmit: true,
  skipLibCheck: true,
  types: [],
  lib: ["lib.es2020.d.ts"],
};

const host = {
  getScriptFileNames: () => files,
  getScriptVersion: (f) => String(versions.get(f) || 0),
  getScriptSnapshot: (f) => {
    if (!fs.existsSync(f)) return undefined;
    return ts.ScriptSnapshot.fromString(fs.readFileSync(f, "utf8"));
  },
  getCurrentDirectory: () => root,
  getCompilationSettings: () => options,
  getDefaultLibFileName: (o) => ts.getDefaultLibFilePath(o),
  fileExists: ts.sys.fileExists,
  readFile: ts.sys.readFile,
  readDirectory: ts.sys.readDirectory,
  directoryExists: ts.sys.directoryExists,
  getDirectories: ts.sys.getDirectories,
};
const ls = ts.createLanguageService(host, ts.createDocumentRegistry());

function position(file, needle, nth) {
  const text = fs.readFileSync(file, "utf8");
  let at = -1;
  for (let i = 0; i <= (nth || 0); i++) {
    at = text.indexOf(needle, at + 1);
    if (at < 0) throw new Error(`"${needle}" not found in ${file}`);
  }
  return at;
}

const ops = {
  warm() {
    const program = ls.getProgram();
    const checker = program.getTypeChecker();
    // Checking one big file pulls most of the program's types into memory.
    const d = ls.getSemanticDiagnostics(path.join(root, "utilities.ts"));
    return `${program.getSourceFiles().length} files, ${checker.getTypeCount()} types, ${d.length} diagnostics`;
  },
  quickinfo(q) {
    const f = path.join(root, q.file);
    const info = ls.getQuickInfoAtPosition(f, position(f, q.needle, q.nth));
    return info ? ts.displayPartsToString(info.displayParts) : "";
  },
  definition(q) {
    const f = path.join(root, q.file);
    const defs = ls.getDefinitionAtPosition(f, position(f, q.needle, q.nth)) || [];
    return defs.map((d) => `${path.relative(root, d.fileName)}:${d.textSpan.start}`).join("\n");
  },
  references(q) {
    const f = path.join(root, q.file);
    const refs = ls.getReferencesAtPosition(f, position(f, q.needle, q.nth)) || [];
    const by = {};
    for (const r of refs) by[path.relative(root, r.fileName)] = (by[path.relative(root, r.fileName)] || 0) + 1;
    return `${refs.length} references\n` + Object.entries(by).sort((a, b) => b[1] - a[1]).slice(0, 10).map(([k, v]) => `${v} ${k}`).join("\n");
  },
  completions(q) {
    const f = path.join(root, q.file);
    const c = ls.getCompletionsAtPosition(f, position(f, q.needle, q.nth) + q.needle.length, {});
    return c ? c.entries.slice(0, 20).map((e) => e.name).join(" ") : "";
  },
  diagnostics(q) {
    const d = ls.getSemanticDiagnostics(path.join(root, q.file));
    return `${d.length} diagnostics`;
  },
};

const rl = readline.createInterface({ input: process.stdin });
rl.on("line", (line) => {
  let res;
  try {
    const q = JSON.parse(line);
    const fn = ops[q.op];
    if (!fn) throw new Error(`unknown op ${q.op}`);
    res = { ok: true, out: String(fn(q)) };
  } catch (e) {
    res = { ok: false, out: "", err: String(e && e.stack || e) };
  }
  process.stdout.write(JSON.stringify(res) + "\n");
});
