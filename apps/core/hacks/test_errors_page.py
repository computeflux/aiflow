"""临时脚本：用无头浏览器打开 board 的 /errors 页面，捕获控制台/页面错误。
把 dev-workflow.computeflux.ai 的 API 请求转发到本机 8001 端口。
"""
import asyncio
import json
import urllib.request

from playwright.async_api import async_playwright

API_TARGET = "http://127.0.0.1:8001"


async def main():
    async with async_playwright() as p:
        browser = await p.chromium.launch()
        page = await browser.new_page()
        console_errors = []
        page_errors = []

        page.on("console", lambda msg: console_errors.append(f"[console:{msg.type}] {msg.text}"))
        page.on("pageerror", lambda err: page_errors.append(f"[pageerror] {err}"))
        page.on("requestfailed", lambda req: console_errors.append(f"[requestfailed] {req.url} {req.failure}"))

        async def forward(route):
            url = route.request.url
            path = url.split("dev-workflow.computeflux.ai", 1)[1]
            target = API_TARGET + path
            method = route.request.method
            headers = {k: v for k, v in route.request.headers.items() if k.lower() not in ("host", "content-length", "origin", "referer")}
            headers["Host"] = "127.0.0.1:8001"
            body = route.request.post_data
            try:
                req = urllib.request.Request(target, data=body.encode() if body else None, method=method, headers=headers)
                with urllib.request.urlopen(req, timeout=10) as resp:
                    data = resp.read()
                    ct = resp.headers.get("Content-Type", "application/json")
                    await route.fulfill(status=resp.status, headers={"Content-Type": ct}, body=data)
            except Exception as e:
                await route.fulfill(status=502, content_type="application/json", body=json.dumps({"error": str(e)}))

        await page.route("**/dev-workflow.computeflux.ai/**", forward)

        try:
            await page.goto("http://127.0.0.1:5173/errors", wait_until="networkidle", timeout=15000)
        except Exception as e:
            console_errors.append(f"[goto] {e}")

        await asyncio.sleep(1)
        content = await page.content()
        body_text = await page.inner_text("body")
        print("=== PAGE ERRORS ===")
        for e in page_errors:
            print(e)
        print("=== CONSOLE ===")
        for e in console_errors:
            print(e)
        print("=== BODY TEXT (first 1500 chars) ===")
        print(body_text[:1500])
        await browser.close()


asyncio.run(main())
