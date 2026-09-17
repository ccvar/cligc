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

  // 权限全选 / 全不选
  //
  // 一个按钮而不是两个：八个框里勾了三个时，"全选"和"全不选"并排放着，
  // 人还得先判断该点哪个。按当前状态切换，永远只有一个决定要做。
  for (const fs of document.querySelectorAll('[data-scopes]')) {
    const btn = fs.querySelector('[data-scope-toggle]');
    const boxes = [...fs.querySelectorAll('input[type="checkbox"]')];
    if (!btn || !boxes.length) continue;

    const sync = () => {
      const allOn = boxes.every((b) => b.checked);
      btn.textContent = allOn ? btn.dataset.none : btn.dataset.all;
      return allOn;
    };
    btn.addEventListener('click', () => {
      const allOn = boxes.every((b) => b.checked);
      for (const b of boxes) b.checked = !allOn;
      sync();
    });
    fs.addEventListener('change', sync);
    sync();
  }

  // 表格拖动排序
  //
  // 用原生 HTML5 拖放，不引库。只改 DOM 顺序和隐藏字段的排列，落库要点
  // 保存——拖一下就发一次请求的话，中途松手、误拖都会立刻写进库。
  //
  // 没有 JS 时这段不运行，每行的排序数字框照常可用，两条路通向同一个结果。
  for (const form of document.querySelectorAll('[data-reorder]')) {
    const body = form.querySelector('[data-reorder-rows]');
    const bar = form.querySelector('[data-reorder-bar]');
    if (!body) continue;

    let dragging = null;
    let touched = false;

    for (const row of body.querySelectorAll('tr')) {
      const handle = row.querySelector('[data-drag]');
      if (!handle) continue;
      // 只有按住手柄才能拖：整行可拖的话，在输入框里选文字会变成拖行。
      handle.addEventListener('mousedown', () => { row.draggable = true; });
      handle.addEventListener('mouseup', () => { row.draggable = false; });

      row.addEventListener('dragstart', (e) => {
        dragging = row;
        row.classList.add('dragging');
        e.dataTransfer.effectAllowed = 'move';
        // Firefox 不设 data 就不触发 drop
        e.dataTransfer.setData('text/plain', '');
      });
      row.addEventListener('dragend', () => {
        row.classList.remove('dragging');
        row.draggable = false;
        dragging = null;
      });
      row.addEventListener('dragover', (e) => {
        if (!dragging || dragging === row) return;
        e.preventDefault();
        const box = row.getBoundingClientRect();
        // 过了中线才换位，否则在边界上会来回抖
        const after = e.clientY > box.top + box.height / 2;
        body.insertBefore(dragging, after ? row.nextSibling : row);
        if (!touched) { touched = true; if (bar) bar.hidden = false; }
      });
    }

    // 拖动改的是 DOM 顺序，而表单提交按的是 hidden input 在文档里的顺序，
    // 两者本来就一致——不需要再同步一遍。
    form.addEventListener('submit', () => { touched = false; });
  }

  // 5. 站点设置：勾上一个语言，它那份站名/描述当场出现。
  //    这两块在页面上隔着一段距离，不联动的话得先保存一次、等页面
  //    重画出来才能填——而人是在勾的那一刻想填的。
  for (const cb of document.querySelectorAll('[data-langpick]')) {
    cb.addEventListener('change', () => {
      const box = document.querySelector(`[data-langblock="${cb.value}"]`);
      if (box && !cb.checked) box.setAttribute('data-langoff', '');
      if (box && cb.checked) {
        box.removeAttribute('data-langoff');
        const first = box.querySelector('input:not([disabled])');
        if (first) first.focus({ preventScroll: true });
        box.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
      }
      // 只剩一种语言时，"这一份是哪个语言的"不是问题，那层小标题和
      // 竖线就只是噪音。勾上第二种的那一刻才让它们出现。
      const form = cb.closest('form');
      if (!form) return;
      const on = form.querySelectorAll('.sitecopy:not([data-langoff])').length;
      form.toggleAttribute('data-multilang', on > 1);
    });
  }
})();
