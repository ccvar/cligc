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


  // 4d. 改过才让保存。
  //
  // 服务端渲染出来的按钮是可用（且可见）的——没有 JS 时一切照旧。
  //
  // 两种表现分别对应两种位置：编辑器的保存按钮在顶部工具条里，藏起来
  // 会让整条按钮往左跳，所以只置灰；站点设置里每块自己一个按钮，没改动
  // 时它就是噪音，直接不出现更干净。
  wireDirty(document.querySelector('form[data-autosave]'),
    document.querySelector('[data-dirty-save]:not([form])'), false);
  for (const form of document.querySelectorAll('[data-dirty-form]')) {
    wireDirty(form, form.querySelector('[data-dirty-save]')
      || document.querySelector(`[data-dirty-save][form="${form.id}"]`), true);
  }

  function wireDirty(f, btn, hide) {
    if (!f || !btn) return;
    const snap = () => [...f.elements]
      .filter((e) => e.name)
      .map((e) => (e.type === 'checkbox' || e.type === 'radio'
        ? e.name + '\u0001' + e.checked
        : e.name + '\u0001' + e.value))
      .join('\u0000');
    let saved = snap();
    const sync = () => {
      const clean = snap() === saved;
      if (hide) { btn.hidden = clean; } else { btn.disabled = clean; }
    };
    sync();
    f.addEventListener('input', sync);
    f.addEventListener('change', sync);
    // 提交时先放开：藏起来或禁用的按钮不会把自己的 name/value 带进请求，
    // 而且提交失败退回来时按钮不该是死的。
    f.addEventListener('submit', () => { btn.hidden = false; btn.disabled = false; });
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
      const box = document.querySelector(`.sitecopy[data-lang="${cb.value}"]`);
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

  // 6. 选图：封面和正文插图共用一个弹窗。
  //
  // 两件事要的都是"从库里挑一张，或者当场传一张"。做成两套只会有两套 bug。
  const picker = document.querySelector('[data-mediapick]');
  if (picker) {
    const grid = picker.querySelector('[data-mediapick-grid]');
    const msg = picker.querySelector('[data-mediapick-msg]');
    const file = picker.querySelector('[data-mediapick-file]');
    const t = picker.dataset;
    let onPick = null;
    let loaded = false;

    const say = (text) => {
      msg.textContent = text || '';
      msg.hidden = !text;
    };

    const tile = (it) => {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'media-pick-item';
      b.title = it.name;
      const img = document.createElement('img');
      img.src = it.url;
      img.alt = it.name;
      img.loading = 'lazy';
      img.decoding = 'async';
      if (it.w) { img.width = it.w; img.height = it.h; }
      b.appendChild(img);
      b.addEventListener('click', () => {
        picker.close();
        if (onPick) onPick(it);
      });
      return b;
    };

    const render = (items) => {
      grid.replaceChildren(...items.map(tile));
      if (!items.length) say(t.empty);
    };

    const load = async () => {
      if (loaded) return;
      try {
        const r = await fetch('/admin/media.json', { headers: { Accept: 'application/json' } });
        const d = await r.json();
        loaded = true;
        render(d.items || []);
      } catch (_) { say(t.failed); }
    };

    // 上传走同一条口子：弹窗里的按钮、拖进正文、粘贴，三处都调它。
    const upload = async (files) => {
      const fd = new FormData();
      for (const f of files) fd.append('file', f);
      const r = await fetch('/admin/media.json', { method: 'POST', body: fd });
      const d = await r.json().catch(() => ({}));
      if (!r.ok || !d.items || !d.items.length) {
        throw new Error(d.error || t.failed);
      }
      return d.items;
    };

    file.addEventListener('change', async () => {
      if (!file.files.length) return;
      say(t.uploading);
      try {
        const items = await upload(file.files);
        loaded = false;
        await load();
        say('');
        // 只传了一张就直接用它——多按一下"选择"没有任何信息量。
        if (items.length === 1) { picker.close(); if (onPick) onPick(items[0]); }
      } catch (e) {
        say(t.errPrefix.replace('{0}', e.message));
      }
      file.value = '';
    });

    picker.querySelector('[data-mediapick-close]')
      .addEventListener('click', () => picker.close());
    picker.addEventListener('click', (e) => { if (e.target === picker) picker.close(); });

    const open = (cb) => { onPick = cb; say(''); picker.showModal(); load(); };

    // --- 封面 ---
    const cover = document.querySelector('[data-cover]');
    if (cover) {
      const sel = cover.querySelector('[data-cover-select]');
      const prev = cover.querySelector('[data-cover-preview]');
      const img = cover.querySelector('[data-cover-img]');
      const acts = cover.querySelector('[data-cover-acts]');
      const pick = cover.querySelector('[data-cover-pick]');
      const clear = cover.querySelector('[data-cover-clear]');
      const altField = document.querySelector('[data-cover-altfield]');

      // 原生下拉是关掉 JS 时的退路，有 JS 就把它藏起来——但它仍然是
      // 真正被提交的那个控件，缩略图只是套在外面的一层。
      sel.hidden = true;
      acts.hidden = false;

      const sync = () => {
        const o = sel.selectedOptions[0];
        const url = o && o.dataset.url;
        prev.hidden = !url;
        clear.hidden = !url;
        if (altField) altField.hidden = !url;
        pick.textContent = url ? t.change : t.choose;
        if (url) {
          img.src = url;
          if (o.dataset.w) { img.width = o.dataset.w; img.height = o.dataset.h; }
        }
      };

      pick.addEventListener('click', () => open((it) => {
        let o = [...sel.options].find((x) => x.value === String(it.id));
        if (!o) {
          o = new Option(it.name, String(it.id));
          o.dataset.url = it.url;
          o.dataset.w = it.w;
          o.dataset.h = it.h;
          sel.add(o, 1);
        }
        sel.value = String(it.id);
        sync();
        sel.dispatchEvent(new Event('change', { bubbles: true }));
      }));

      clear.addEventListener('click', () => {
        sel.value = '';
        sync();
        sel.dispatchEvent(new Event('change', { bubbles: true }));
      });

      sync();
    }

    // --- 正文插图 ---
    const body = document.querySelector('[data-imagedrop]');
    if (body) {
      // 插在光标处，并把光标停在 ![] 的方括号中间：紧接着要写的就是
      // 替代文字，而它是最容易被跳过、也最不该跳过的那一格。
      const insert = (it) => {
        const md = '![](' + it.url + ')';
        const at = body.selectionStart;
        const before = body.value.slice(0, at);
        const after = body.value.slice(body.selectionEnd);
        const pad = before && !before.endsWith('\n') ? '\n\n' : '';
        body.value = before + pad + md + after;
        const caret = at + pad.length + 2;
        body.focus();
        body.setSelectionRange(caret, caret);
        body.dispatchEvent(new Event('input', { bubbles: true }));
      };

      const btn = document.querySelector('[data-insert-image]');
      if (btn) btn.addEventListener('click', () => open(insert));

      const take = async (files) => {
        const imgs = [...files].filter((f) => f.type.startsWith('image/'));
        if (!imgs.length) return false;
        body.classList.add('busy');
        try {
          for (const it of await upload(imgs)) insert(it);
          loaded = false;
        } catch (e) {
          alert(t.errPrefix.replace('{0}', e.message));
        }
        body.classList.remove('busy');
        return true;
      };

      body.addEventListener('dragover', (e) => {
        if (e.dataTransfer && [...e.dataTransfer.types].includes('Files')) {
          e.preventDefault();
          body.classList.add('dropping');
        }
      });
      body.addEventListener('dragleave', () => body.classList.remove('dropping'));
      body.addEventListener('drop', (e) => {
        if (!e.dataTransfer || !e.dataTransfer.files.length) return;
        e.preventDefault();
        body.classList.remove('dropping');
        take(e.dataTransfer.files);
      });
      // 粘贴：剪贴板里同时有图和文字时（从网页复制常常如此），有图就走图。
      body.addEventListener('paste', (e) => {
        if (!e.clipboardData || !e.clipboardData.files.length) return;
        const imgs = [...e.clipboardData.files].filter((f) => f.type.startsWith('image/'));
        if (!imgs.length) return;
        e.preventDefault();
        take(imgs);
      });
    }
  }

  // 7. 按语言分页：站点文案、分类表格、新建分类弹窗，三处共用。
  //
  // 都是同一件事——一组按语言标记的块/格子，一次只显示一种。长相共用
  // 也是有意的：站长在一个后台里看到两种不同的标签条，会以为它们是
  // 两种不同的东西。
  //
  // 注意不能写进上面那段选图的 if (picker) {} 里：站点设置页没有选图
  // 弹窗，picker 是 null，整块就一次都不会跑。
  function langPager(tabs, items, label) {
    if (!tabs) return null;
    const visible = () => items().filter((el) => !el.hasAttribute('data-langoff'));
    let active = null;

    const show = (code) => {
      active = code;
      for (const el of items()) el.classList.toggle('is-hidden', el.dataset.lang !== code);
      for (const t of tabs.children) {
        const on = t.dataset.tab === code;
        t.classList.toggle('on', on);
        t.setAttribute('aria-selected', on ? 'true' : 'false');
      }
    };

    const build = () => {
      const list = visible();
      const codes = [...new Set(list.map((el) => el.dataset.lang))];
      // 只有一种语言时不需要标签条，也不该把那一块藏起来。
      if (codes.length < 2) {
        tabs.hidden = true;
        tabs.replaceChildren();
        for (const el of items()) el.classList.remove('is-hidden');
        active = null;
        return;
      }
      tabs.hidden = false;
      tabs.replaceChildren(...codes.map((code) => {
        const t = document.createElement('button');
        t.type = 'button';
        t.dataset.tab = code;
        t.setAttribute('role', 'tab');
        t.lang = code;
        t.textContent = label(list.find((el) => el.dataset.lang === code)) || code;
        t.addEventListener('click', () => show(code));
        return t;
      }));
      // 原来停在哪一页就还停在哪一页；那一页没了才回到第一页。
      show(codes.includes(active) ? active : codes[0]);
    };

    build();
    return { build, show };
  }

  // 每个参与分页的元素统一用 data-lang 标语言，langPager 只认这一个属性。
  const taggedBlocks = (sel) => () => [...document.querySelectorAll(sel)];

  // 7a. 站点设置的文案分页
  const copyPager = langPager(
    document.querySelector('[data-langtabs]'),
    taggedBlocks('.sitecopy'),
    (el) => el.querySelector('.sitecopy-head [lang]')?.textContent.trim());
  if (copyPager) {
    // 勾选语言会增减块，标签条要跟着重建。
    for (const cb of document.querySelectorAll('[data-langpick]')) {
      cb.addEventListener('change', () => {
        copyPager.build();
        if (cb.checked) copyPager.show(cb.value);
      });
    }
  }

  // 7b. 分类表格里每行的那一对格子
  langPager(document.querySelector('[data-langcells]'),
    taggedBlocks('[data-langcell]'),
    (el) => el.querySelector('.lang-tag')?.textContent.trim());

  // 7c. 新建分类弹窗
  const newPager = langPager(document.querySelector('[data-newtabs]'),
    taggedBlocks('[data-newlang]'),
    (el) => el.querySelector('.lang-tag')?.textContent.trim());
  const newForm = document.querySelector('[data-newcat]');
  if (newPager && newForm) {
    // 必填项在没显示的那一页上时，浏览器会拒绝提交并报
    // "invalid form control is not focusable"——什么都不显示。
    // 先切到出问题的那一页，再让浏览器提示。
    newForm.addEventListener('invalid', (e) => {
      const block = e.target.closest('[data-newlang]');
      if (block) newPager.show(block.dataset.lang);
    }, true);
  }

})();
