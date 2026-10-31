#!/usr/bin/env node
// 检查 web/index.html 中内联 JavaScript 的语法。
//
// 背景：前端没有构建步骤，`<script>` 里的代码直接由浏览器执行——
// 一个多余的大括号只会在用户打开页面时才暴露。CI 里做一次语法检查
// 是最低成本的安全网。
//
// 实现：用 node 的 vm.Script 编译（只编译、不执行），语法错误会抛异常。
//
// 用法：node scripts/check-web-js.mjs [path/to/index.html]

import { readFileSync } from 'node:fs';
import { Script } from 'node:vm';

const target = process.argv[2] ?? 'web/index.html';

let html;
try {
  html = readFileSync(target, 'utf8');
} catch (err) {
  console.error(`读取失败：${target}：${err.message}`);
  process.exit(1);
}

// 只处理不带属性的 <script> 块（本项目没有 src= 外链脚本）。
const blocks = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map((m) => m[1]);

if (blocks.length === 0) {
  console.log(`${target}：未找到内联 <script>，跳过`);
  process.exit(0);
}

let failed = 0;
blocks.forEach((code, i) => {
  const lineOffset = html.slice(0, html.indexOf(code)).split('\n').length - 1;
  try {
    // 编译而不执行：构造函数会做完整的语法解析。
    new Script(code, { filename: `${target}#script${i + 1}` });
  } catch (err) {
    failed++;
    console.error(`\n✗ ${target} 第 ${i + 1} 个 <script> 块语法错误：`);
    console.error(`  起始行约 ${lineOffset}`);
    console.error(`  ${err.message}`);
  }
});

if (failed > 0) {
  console.error(`\n共 ${failed} 个脚本块未通过语法检查`);
  process.exit(1);
}

console.log(`${target}：${blocks.length} 个内联 <script> 块语法正确`);
