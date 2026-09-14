import assert from "node:assert/strict";
import test from "node:test";
import { JSDOM } from "jsdom";
import { setLocale, t, translateDom } from "../src/i18n.ts";

test("i18n translates visible labels and reverses without losing the source text", () => {
  const previous = Object.getOwnPropertyDescriptor(globalThis, "document");
  const dom = new JSDOM('<main><button title="Refresh">Refresh</button><p>New chat</p></main>');
  Object.defineProperty(globalThis, "document", { configurable: true, value: dom.window.document });
  try {
    setLocale("zh-CN");
    translateDom(dom.window.document.body);
    assert.equal(dom.window.document.querySelector("button")?.textContent, "刷新");
    assert.equal(dom.window.document.querySelector("button")?.getAttribute("title"), "刷新");
    assert.equal(dom.window.document.querySelector("p")?.textContent, "新聊天");

    setLocale("en-US");
    translateDom(dom.window.document.body);
    assert.equal(dom.window.document.querySelector("button")?.textContent, "Refresh");
    assert.equal(dom.window.document.querySelector("p")?.textContent, "New chat");
  } finally {
    if (previous) Object.defineProperty(globalThis, "document", previous);
    else delete (globalThis as { document?: Document }).document;
    setLocale("en-US");
  }
});

test("i18n translates dynamic relative-time patterns and interpolated labels", () => {
  setLocale("zh-CN");
  assert.equal(t("5m ago"), "5 分钟前");
  assert.equal(t("New chat in {name}", { name: "项目 A" }), "在 项目 A 中新建聊天");
  assert.equal(t("Knowledge: ready"), "知识：已就绪");
  assert.equal(t("3 pages"), "3 个页面");
  assert.equal(t("notes.md added to this project."), "notes.md 已添加到此项目。");
  setLocale("en-US");
  assert.equal(t("New chat in {name}", { name: "Project A" }), "New chat in Project A");
});

test("i18n does not rewrite attributes that are already translated", () => {
  const previous = Object.getOwnPropertyDescriptor(globalThis, "document");
  const dom = new JSDOM('<button title="Refresh" aria-label="Refresh">Refresh</button>');
  Object.defineProperty(globalThis, "document", { configurable: true, value: dom.window.document });
  try {
    setLocale("zh-CN");
    translateDom(dom.window.document.body);

    const button = dom.window.document.querySelector("button");
    assert.ok(button);
    let attributeWrites = 0;
    const setAttribute = button.setAttribute.bind(button);
    button.setAttribute = (name: string, value: string): void => {
      attributeWrites += 1;
      setAttribute(name, value);
    };

    translateDom(dom.window.document.body);
    assert.equal(attributeWrites, 0);
  } finally {
    if (previous) Object.defineProperty(globalThis, "document", previous);
    else delete (globalThis as { document?: Document }).document;
    setLocale("en-US");
  }
});
