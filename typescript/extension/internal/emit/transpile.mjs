// Reads {"configs": {"<id>": <compilerOptions>}, "files": [{"path", "loader",
// "config"}]} on stdin and writes {"<path>": "<sha256 of the emitted JS>"} on
// stdout. Each file is transpiled by Bun's own transpiler, the one `bun test`
// runs a TypeScript module through, with the compiler options of the tsconfig
// that governs it. A file that does not transpile gets no entry: the caller
// then hashes its raw bytes.
const input = JSON.parse(await Bun.stdin.text());
const transpilers = new Map();
const transpiler = (config, loader) => {
  const id = `${config}\u0000${loader}`;
  let t = transpilers.get(id);
  if (!t) {
    t = new Bun.Transpiler({
      loader,
      tsconfig: { compilerOptions: input.configs[config] ?? {} },
    });
    transpilers.set(id, t);
  }
  return t;
};
const out = {};
for (const file of input.files) {
  try {
    const js = transpiler(file.config, file.loader).transformSync(await Bun.file(file.path).text());
    out[file.path] = new Bun.CryptoHasher('sha256').update(js).digest('hex');
  } catch {
    // Left out: the caller keys the file on its bytes.
  }
}
process.stdout.write(JSON.stringify(out));
