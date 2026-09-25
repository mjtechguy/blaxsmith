import assert from "node:assert/strict";
import { test } from "node:test";
import { createServer } from "vite";

test("interaction Markdown renders formatting and strips anything executable", async () => {
  const saved = { window: globalThis.window, localStorage: globalThis.localStorage, matchMedia: globalThis.matchMedia };
  globalThis.window = { location: { origin: "https://blaxsmith.test" } };
  globalThis.localStorage = { getItem: () => null, setItem: () => {} };
  globalThis.matchMedia = () => ({ matches: false, addEventListener() {}, removeEventListener() {} });
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    const { renderMarkdown: render } = await server.ssrLoadModule("/tests/render-app.tsx");
    const { safeHref } = await server.ssrLoadModule("/src/markdown.tsx");

    const html = await render("**Bold** and `code` in docs/spec.md\n\n- CSV\n- JSON\n\n[spec](https://example.com/spec) · [mail](mailto:a@example.com)\n\n# Big heading");
    assert.match(html, /<strong>Bold<\/strong>/);
    assert.match(html, /<code>code<\/code>/);
    assert.match(html, /<ul><li>CSV<\/li><li>JSON<\/li><\/ul>/);
    assert.match(html, /<a href="https:\/\/example.com\/spec" target="_blank" rel="noopener noreferrer">spec<\/a>/);
    assert.match(html, /<a href="mailto:a@example.com" target="_blank" rel="noopener noreferrer">mail<\/a>/);
    assert.doesNotMatch(html, /<h1/, "agent headings never add to the page outline");

    const hostile = await render([
      "<script>alert(1)</script>",
      "<img src=x onerror=alert(2)>",
      "inline <img src=x onerror=alert(3)> and <b onclick=alert(4)>bold</b>",
      "[click](javascript:alert(5)) [data](data:text/html,<script>alert(6)</script>) [vb](vbscript:msgbox) [rel](/admin)",
      "![pixel](https://tracker.example/p.png)",
      "<a href=\"javascript:alert(7)\">raw link</a>",
      "<iframe src=\"https://evil.example\"></iframe>",
    ].join("\n\n"));
    assert.doesNotMatch(hostile, /<script|<img|<iframe|<b |onerror|onclick|javascript:|data:text|vbscript:|href="\/admin"/i, hostile);
    assert.doesNotMatch(hostile, /<a /, "no link survives without an http, https, or mailto target");
    assert.match(hostile, /click/, "unsafe links keep their text");
    assert.match(hostile, /pixel/, "images show alt text instead of loading");

    assert.equal((await render("5 &lt; 6 &amp;&amp; 7 &gt; 3")).includes("5 &lt; 6 &amp;&amp; 7 &gt; 3"), true, "entities decode once, then React escapes");
    assert.equal(safeHref("https://example.com/a?b=c"), "https://example.com/a?b=c");
    for (const bad of ["javascript:alert(1)", " JAVASCRIPT:alert(1)", "data:text/html,x", "/relative", "//evil.example", "file:///etc/passwd"]) {
      assert.equal(safeHref(bad), null, bad);
    }
  } finally {
    await server.close();
    Object.assign(globalThis, saved);
  }
});
