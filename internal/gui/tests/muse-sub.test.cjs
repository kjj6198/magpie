// Run with Node's test runner and Playwright on the module path; see README.md.
// Muse Code (Meta) is a subscription magpie serves with the muse CLI's own
// sign-in, and signs in nowhere itself. Its row says to run `muse login`;
// starting it first says Meta may act on use outside Muse Code, then shows
// the command to run, asking nothing of the backend's sign-in. Check again
// says when Muse isn't signed in yet, and closes once its account is there.
// English and Chinese; no backend, the API is faked here.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");
const openai = { id: "openai", name: "OpenAI", icon: "openai", preset: "openai", models: [], agents: [], key: { set: true, masked: "sk-…ab12" } };
const muse = { id: "meta-muse", name: "Muse Code", icon: "meta-color", models: [], agents: [],
  account: { agent: "meta-muse", user: "me@example.com", logins: [{ agent: "meta-muse", user: "me@example.com", active: true, on: true, own: true }] } };

function server(lang, state, asked) {
  return async (route) => {
    const url = new URL(route.request().url());
    const json = (data) => route.fulfill({ json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:"${lang}",theme:"light",web:false};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang, theme: "light" } });
    if (url.pathname === "/api/providers") return json({ providers: state.list, presets: [], excluded: [], gateway: { running: true, window: true } });
    if (url.pathname === "/api/groups") return json({ groups: [], pools: [] });
    if (url.pathname === "/api/gateway/trace") return json({ routes: [] });
    if (url.pathname.startsWith("/api/signin")) {
      asked.push(url.pathname);
      return json({ id: "s1", agent: "meta-muse", state: "failed", error: "magpie can't sign in to meta-muse accounts" });
    }
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    try { await route.fulfill({ body: await fs.readFile(file), contentType }); } catch { await route.fulfill({ status: 404, body: "" }); }
  };
}

const L = {
  en: { hint: /Install Muse Code and run muse login in a terminal/, risk: "Muse Code accounts can be suspended", note: /Meta's terms keep a Muse Code subscription to Muse Code itself/,
    anyway: "Sign in anyway", cli: "Sign in to Muse Code in its CLI", again: "Check again", notYet: /Muse Code isn't signed in yet/ },
  zh: { hint: /安装 Muse Code，在终端运行 muse login/, risk: "Muse Code 账号可能被封禁", note: /Meta 的条款将 Muse Code 订阅限定在 Muse Code 本身使用/,
    anyway: "仍然登录", cli: "在 Muse Code 的 CLI 中登录", again: "重新检测", notYet: /Muse Code 尚未登录/ },
};

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(engine + ": Muse Code is signed in in its own CLI", async (t) => {
    assert(["chromium", "webkit"].includes(engine), "BROWSER must be chromium or webkit");
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const lang of ["en", "zh"]) {
      await t.test(lang, async () => {
        const w = L[lang];
        const state = { list: [openai] }, asked = [];
        const page = await (await browser.newContext({ viewport: { width: 900, height: 800 } })).newPage();
        const errors = [];
        page.on("pageerror", (e) => errors.push(e.message));
        await page.route("**/*", server(lang, state, asked));
        await page.goto("http://magpie.test/?view=providers");
        await page.locator("#addProvider").click();
        const sheet = page.locator("#addSheet");
        const row = sheet.locator('.tile[data-pick="Muse Code"]');
        await row.waitFor();
        assert.match(await row.getAttribute("title"), w.hint);

        const y = await page.evaluate(() => scrollY);
        await row.click();
        let box = sheet.locator(".signing");
        await box.waitFor();
        assert.equal(await page.evaluate(() => scrollY), y, "the click doesn't scroll");
        assert.equal(await box.locator(".n").textContent(), w.risk);
        assert.match(await box.textContent(), w.note);

        await box.locator("button", { hasText: w.anyway }).click();
        await box.locator(".n", { hasText: w.cli }).waitFor();
        assert.equal(await box.locator("code").textContent(), "muse login");

        await box.locator("button", { hasText: w.again }).click();
        await box.locator(".s", { hasText: w.notYet }).waitFor();

        state.list = [openai, muse];
        await box.locator("button", { hasText: w.again }).click();
        await box.waitFor({ state: "detached" });
        assert.deepEqual(asked, [], "magpie's own sign-in is never started");
        assert.deepEqual(errors, []);
        await page.context().close();
      });
    }
  });
}
