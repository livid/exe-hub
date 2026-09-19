// node testdata/draw.js — draws identicon.json with the Hub app's own identicon(): its source is cut
// out of the sysapp and run against a canvas that records what it fills
const fs = require("fs");
const src = fs.readFileSync("/www/exe/internal/server/sysapps/hub/index.html", "utf8");
const m = src.match(/function identicon\(id\) \{[\s\S]*?\n\}/);
if (!m) throw new Error("identicon() not found");
const identicon = new Function("el", m[0] + "; return identicon;")((tag, a) => {
  const px = Array.from({ length: a.height }, () => Array(a.width).fill("."));
  let fill = "";
  const fills = [];
  return { px, fills, getContext: () => ({
    set fillStyle(v) { fill = v; fills.push(v); }, get fillStyle() { return fill; },
    fillRect(x, y, w, h) { for (let j = y; j < y + h; j++) for (let i = x; i < x + w; i++) px[j][i] = fills.length > 1 ? "#" : "."; },
  }) };
});
const ids = ["fa0fd0d0cbc2e8d1", "9bf553faa643997d", "44314766ad285c2a", "0000000000000000", "ffffffffffffffff",
  "1111111111111110", "0123456789abcdef", "fedcba9876543210", "a1b2c3d4e5f60718", "5e5e5e5e5e5e5e5e", "02468ace13579bdf", "7f3c9a1e0b5d2486"];
const out = ids.map(id => { const c = identicon(id); return { id, ground: c.fills[0], ink: c.fills[1], rows: c.px.map(r => r.join("")) }; });
fs.mkdirSync("/www/exe-hub/internal/identicon/testdata", { recursive: true });
fs.writeFileSync("/www/exe-hub/internal/identicon/testdata/identicon.json", JSON.stringify(out, null, 1).replace(/\[\n\s+("[.#]{5}"),\n\s+("[.#]{5}"),\n\s+("[.#]{5}"),\n\s+("[.#]{5}"),\n\s+("[.#]{5}")\n\s+\]/g, "[$1, $2, $3, $4, $5]") + "\n");
console.log(fs.readFileSync("/www/exe-hub/internal/identicon/testdata/identicon.json", "utf8"));
