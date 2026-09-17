// admin.js —— 只在 /admin 和 /login 加载。
//
// 编辑器本地暂存、改动才让保存、生成 IndexNow 密钥。
// 公开页面不会加载这个文件。
(() => {
  'use strict';

  // 2. 编辑器本地暂存：浏览器崩了或误关标签页时能找回正文。
  //    只存在本机 localStorage，不上传；提交成功后清掉。
  const form = document.querySelector('form[data-autosave]');
  if (form) {
    const key = 'cligc:draft:' + location.pathname;
    const fields = ['title', 'body_md', 'summary', 'tags'];
    const read = () => Object.fromEntries(
      fields.map(n => [n, form.elements[n] ? form.elements[n].value : '']));

    try {
      const saved = JSON.parse(localStorage.getItem(key) || 'null');
      if (saved && saved.body_md && saved.body_md !== form.elements.body_md.value) {
        if (confirm('检测到本机未提交的修改，要恢复吗？')) {
          for (const n of fields) if (form.elements[n]) form.elements[n].value = saved[n] || '';
        } else {
          localStorage.removeItem(key);
        }
      }
    } catch (_) { /* localStorage 不可用时静默跳过 */ }

    let timer;
    form.addEventListener('input', () => {
      clearTimeout(timer);
      timer = setTimeout(() => {
        try { localStorage.setItem(key, JSON.stringify(read())); } catch (_) {}
      }, 800);
    });
    form.addEventListener('submit', () => {
      try { localStorage.removeItem(key); } catch (_) {}
    });
  }


  // 4d. 改过才让保存
  //
  // 服务端渲染出来的按钮是可用的——没有 JS 时一切照旧。这里只是在没有
  // 任何改动时把它置灰：一个永远亮着的"保存"没法回答"我刚才那下存了吗"。
  {
    const f = document.querySelector('form[data-autosave]');
    const btn = document.querySelector('[data-dirty-save]');
    if (f && btn) {
      const snap = () => [...f.elements]
        .filter((e) => e.name)
        .map((e) => (e.type === 'checkbox' || e.type === 'radio'
          ? e.name + '\u0001' + e.checked
          : e.name + '\u0001' + e.value))
        .join('\u0000');
      let saved = snap();
      const sync = () => { btn.disabled = snap() === saved; };
      sync();
      f.addEventListener('input', sync);
      f.addEventListener('change', sync);
      // 提交时先放开：禁用状态的按钮不会把自己的 name/value 带进请求，
      // 而且提交失败退回来时按钮不该是死的。
      f.addEventListener('submit', () => { btn.disabled = false; });
    }
  }


  // 4e. 生成 IndexNow 密钥
  //
  // 密钥没有任何语义，就是一串随机十六进制——让站长自己想一个只会得到
  // "mysite123"这种。在浏览器里用 crypto 生成，不必往服务端多跑一趟。
  for (const btn of document.querySelectorAll('[data-genkey-btn]')) {
    btn.addEventListener('click', () => {
      const input = btn.closest('.with-action')?.querySelector('[data-genkey]');
      if (!input) return;
      const b = new Uint8Array(16);
      crypto.getRandomValues(b);
      input.value = [...b].map((x) => x.toString(16).padStart(2, '0')).join('');
      input.dispatchEvent(new Event('input', { bubbles: true }));
    });
  }
})();
